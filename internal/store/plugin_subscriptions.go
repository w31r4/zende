package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
)

const MaxPluginSubscriptionEvents = 4096
const MaxPluginSubscriptionBytes = 32 << 20
const MaxPluginSubscriptionChanges = 16384
const MaxPluginEventRecordBytes = plugin.MaxEventBytes + 512

// Current state is the last immutable history entry. Transport acknowledgements
// live here, not in the Core stream to which a plugin can itself subscribe.
const pluginSubscriptionSchema = `
CREATE TABLE plugin_subscriptions (
 subscription_id TEXT PRIMARY KEY REFERENCES operations(id) DEFERRABLE INITIALLY DEFERRED,
 agent_id TEXT NOT NULL REFERENCES agents(id), owner TEXT NOT NULL, intent TEXT NOT NULL, digest TEXT NOT NULL
);
CREATE INDEX plugin_subscriptions_agent ON plugin_subscriptions(agent_id,subscription_id);
CREATE TABLE plugin_subscription_history (
 subscription_id TEXT NOT NULL REFERENCES plugin_subscriptions(subscription_id), sequence INTEGER NOT NULL,
 payload TEXT NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(subscription_id,sequence)
);
CREATE TABLE plugin_subscription_events (
 subscription_id TEXT NOT NULL REFERENCES plugin_subscriptions(subscription_id), sequence INTEGER NOT NULL,
 payload TEXT NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(subscription_id,sequence)
);
CREATE VIEW plugin_subscription_heads AS
 SELECT s.subscription_id,s.agent_id,s.owner,h.sequence,h.payload,h.digest FROM plugin_subscriptions s
 JOIN plugin_subscription_history h ON h.subscription_id=s.subscription_id
 AND h.sequence=(SELECT max(sequence) FROM plugin_subscription_history WHERE subscription_id=s.subscription_id);
CREATE TRIGGER plugin_subscriptions_no_update BEFORE UPDATE ON plugin_subscriptions BEGIN SELECT RAISE(ABORT,'immutable subscription'); END;
CREATE TRIGGER plugin_subscriptions_no_delete BEFORE DELETE ON plugin_subscriptions BEGIN SELECT RAISE(ABORT,'retained subscription'); END;
CREATE TRIGGER plugin_subscription_history_no_update BEFORE UPDATE ON plugin_subscription_history BEGIN SELECT RAISE(ABORT,'append-only subscription history'); END;
CREATE TRIGGER plugin_subscription_history_no_delete BEFORE DELETE ON plugin_subscription_history BEGIN SELECT RAISE(ABORT,'retained subscription history'); END;
CREATE TRIGGER plugin_subscription_events_no_update BEFORE UPDATE ON plugin_subscription_events BEGIN SELECT RAISE(ABORT,'append-only plugin events'); END;
CREATE TRIGGER plugin_subscription_events_no_delete BEFORE DELETE ON plugin_subscription_events BEGIN SELECT RAISE(ABORT,'retained plugin events'); END;
PRAGMA user_version=16;
`

type PluginSubscriptionIntent struct {
	Request         protocol.Request             `json:"request"`
	Binding         plugin.EventSubscriptionInfo `json:"binding"`
	ResourceVersion string                       `json:"resource_version"`
	Generation      int64                        `json:"generation"`
	Source          *protocol.CallSource         `json:"source,omitempty"`
}

type PluginEventDelivery struct {
	After         int64  `json:"after"`
	Through       int64  `json:"through"`
	SHA256        string `json:"sha256"`
	Confirmed     bool   `json:"confirmed"`
	NotDispatched bool   `json:"not_dispatched,omitempty"`
}

func (d *PluginEventDelivery) Pending() bool { return d != nil && !d.Confirmed && !d.NotDispatched }

type pluginSubscriptionHead struct {
	State            string               `json:"state"`
	Cursor           int64                `json:"cursor"`
	ReceivedBytes    int64                `json:"received_bytes"`
	CloseOperationID string               `json:"close_operation_id,omitempty"`
	Delivery         *PluginEventDelivery `json:"delivery,omitempty"`
	Reason           string               `json:"reason,omitempty"`
}

type PluginSubscriptionReceipt struct {
	plugin.EventSubscriptionInfo
	AgentID          string               `json:"agent_id"`
	ResourceID       string               `json:"resource_id"`
	ResourceVersion  string               `json:"resource_version"`
	Generation       int64                `json:"generation"`
	Source           *protocol.CallSource `json:"source,omitempty"`
	ReceivedBytes    int64                `json:"received_bytes"`
	CloseOperationID string               `json:"close_operation_id,omitempty"`
	Delivery         *PluginEventDelivery `json:"delivery,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	Error            *protocol.Error      `json:"error,omitempty"`
}

type PluginSubscriptionRecord struct {
	Intent   PluginSubscriptionIntent
	Owner    string
	Receipt  PluginSubscriptionReceipt
	head     pluginSubscriptionHead
	revision int64
}

func subscriptionInput(req protocol.Request) (protocol.PluginSubscribe, error) {
	var p protocol.PluginSubscribe
	if req.Validate() != nil || req.Method != "plugin.events.subscribe" || !pluginID(req.OperationID, true) || len(req.Params) > 16<<10 || protocol.StrictDecode(req.Params, &p) != nil || !pluginID(p.AgentID, true) || !pluginID(p.ResourceID, true) || !pluginID(p.DefinitionRef, true) || p.After < 0 || len(p.Kinds) > 32 {
		return p, protocol.E("invalid_params", "plugin subscription requires Agent, resource, definition and a nonnegative cursor")
	}
	if p.Caller != nil && (p.Caller.AgentID != p.AgentID || !pluginID(p.Caller.TaskID, true) || !pluginID(p.Caller.OperationID, true)) {
		return p, protocol.E("invalid_params", "subscription caller must identify this Agent's execution")
	}
	return p, nil
}

func subscriptionSourceInput(p protocol.PluginSubscribe) PluginCallInput {
	return PluginCallInput{AgentID: p.AgentID, ResourceID: p.ResourceID, DefinitionRef: p.DefinitionRef, Caller: p.Caller}
}

func subscriptionTarget(p protocol.PluginSubscribe, req protocol.Request) protocol.PluginSubscriptionTarget {
	return protocol.PluginSubscriptionTarget{AgentID: p.AgentID, SubscriptionID: req.OperationID}
}

func subscriptionActive(state string) bool {
	return state == "opening" || state == "open" || state == "closing"
}

func subscriptionError(h pluginSubscriptionHead) *protocol.Error {
	if h.State == "unknown" {
		return protocol.E("operation_unknown", "plugin subscription outcome is unverified; inspect its original identity, do not repeat it")
	}
	if h.State == "not_dispatched" {
		return protocol.E("plugin_not_dispatched", "plugin subscription was not opened; inspect its original record")
	}
	if h.Reason == "resource_limit" {
		return protocol.E("resource_limit", "plugin subscription reached its retention limit; saved events remain readable")
	}
	return nil
}

func subscriptionReceipt(i PluginSubscriptionIntent, h pluginSubscriptionHead) PluginSubscriptionReceipt {
	var p protocol.PluginSubscribe
	_ = protocol.StrictDecode(i.Request.Params, &p)
	b := i.Binding
	b.State, b.Cursor = h.State, h.Cursor
	return PluginSubscriptionReceipt{EventSubscriptionInfo: b, AgentID: p.AgentID, ResourceID: p.ResourceID, ResourceVersion: i.ResourceVersion, Generation: i.Generation, Source: i.Source, ReceivedBytes: h.ReceivedBytes, CloseOperationID: h.CloseOperationID, Delivery: h.Delivery, Reason: h.Reason, Error: subscriptionError(h)}
}

func subscriptionRecord(ctx context.Context, q rowQuerier, target protocol.PluginSubscriptionTarget) (*PluginSubscriptionRecord, error) {
	var raw, hash, state, stateHash, owner string
	var revision int64
	err := q.QueryRowContext(ctx, `SELECT s.intent,s.digest,s.owner,h.sequence,h.payload,h.digest FROM plugin_subscriptions s JOIN plugin_subscription_heads h USING(subscription_id) WHERE s.agent_id=? AND s.subscription_id=?`, target.AgentID, target.SubscriptionID).Scan(&raw, &hash, &owner, &revision, &state, &stateHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var i PluginSubscriptionIntent
	var h pluginSubscriptionHead
	if len(raw) > maxPluginIntentBytes || pluginDigest([]byte(raw)) != hash || protocol.StrictDecode([]byte(raw), &i) != nil || len(state) > 16<<10 || pluginDigest([]byte(state)) != stateHash || protocol.StrictDecode([]byte(state), &h) != nil {
		return nil, protocol.E("invalid_state", "saved plugin subscription failed integrity verification")
	}
	p, err := subscriptionInput(i.Request)
	b := i.Binding
	if err != nil || subscriptionTarget(p, i.Request) != target || b.OperationID != target.SubscriptionID || b.SubscriptionID != target.SubscriptionID || b.DefinitionRef != p.DefinitionRef || b.Scope.AgentID != p.AgentID || b.Scope.TaskID != subscriptionSourceInput(p).BoundTaskID() || !validPluginSource(subscriptionSourceInput(p), i.Source) || !pluginID(owner, true) || revision < 1 || revision > MaxPluginSubscriptionChanges+3 || h.Cursor < b.After || h.ReceivedBytes < 0 || h.ReceivedBytes > MaxPluginSubscriptionBytes || !pluginID(h.CloseOperationID, false) {
		return nil, protocol.E("invalid_state", "saved plugin subscription identity or cursor changed")
	}
	if b.State != "prepared" || b.Cursor != b.After || b.After != p.After || (b.Direction != "core" && b.Direction != "plugin") || b.Direction == "plugin" && (b.After != 0 || h.Cursor > MaxPluginSubscriptionEvents) || b.Direction == "core" && h.ReceivedBytes != 0 {
		return nil, protocol.E("invalid_state", "saved subscription source or retention metadata changed")
	}
	if !subscriptionActive(h.State) && h.State != "closed" && h.State != "unknown" && h.State != "not_dispatched" || h.State == "closing" && h.CloseOperationID == "" {
		return nil, protocol.E("invalid_state", "saved subscription state is invalid")
	}
	if h.Delivery != nil && (b.Direction != "core" || h.Delivery.After < b.After || h.Delivery.Through <= h.Delivery.After || len(h.Delivery.SHA256) != 64 || h.Delivery.Confirmed && h.Delivery.NotDispatched || h.Delivery.Confirmed && h.Cursor != h.Delivery.Through || !h.Delivery.Confirmed && h.Cursor != h.Delivery.After) {
		return nil, protocol.E("invalid_state", "saved delivery differs from the confirmed cursor")
	}
	return &PluginSubscriptionRecord{Intent: i, Owner: owner, Receipt: subscriptionReceipt(i, h), head: h, revision: revision}, nil
}

func (s *Store) PluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget) (*PluginSubscriptionRecord, error) {
	if !pluginID(target.AgentID, true) || !pluginID(target.SubscriptionID, true) {
		return nil, protocol.E("invalid_params", "Agent and subscription IDs are required")
	}
	return subscriptionRecord(ctx, s.db, target)
}

func ownedSubscription(ctx context.Context, q rowQuerier, target protocol.PluginSubscriptionTarget, owner string) (*PluginSubscriptionRecord, error) {
	r, err := subscriptionRecord(ctx, q, target)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, protocol.E("not_found", "plugin subscription not found for this Agent")
	}
	if r.Owner != owner {
		return nil, protocol.E("operation_conflict", "plugin subscription belongs to another Core instance")
	}
	return r, nil
}

func publishedSubscription(ctx context.Context, q rowQuerier, i PluginSubscriptionIntent) (*plugin.Catalog, error) {
	p, err := subscriptionInput(i.Request)
	if err != nil {
		return nil, err
	}
	var raw string
	err = q.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v JOIN resource_publications p ON p.id=v.id AND p.version=v.version WHERE p.agent_id=? AND p.id=? AND p.version=? AND p.generation=? LIMIT 1`, p.AgentID, p.ResourceID, i.ResourceVersion, i.Generation).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, protocol.E("stale_reference", "subscription version was not published to this Agent")
	}
	if err != nil {
		return nil, err
	}
	pkg, err := decodePackage(raw)
	if err != nil {
		return nil, err
	}
	if pkg.Plugin == nil {
		return nil, protocol.E("invalid_state", "subscription resource is not a plugin")
	}
	m, err := resource.ParsePluginManifest(pkg.Files[0].Data)
	if err != nil {
		return nil, err
	}
	b := i.Binding
	kinds := slices.Clone(p.Kinds)
	slices.Sort(kinds)
	if b.PluginID != m.ID || b.PluginVersion != m.Version || b.ManifestVersion != m.Digest() || b.CatalogVersion != pkg.Plugin.CatalogVersion || !pluginID(b.InstanceID, true) || b.DefinitionRef != p.DefinitionRef || b.OperationID != i.Request.OperationID || b.SubscriptionID != i.Request.OperationID || b.Scope.AgentID != p.AgentID || b.Scope.TaskID != subscriptionSourceInput(p).BoundTaskID() || b.State != "prepared" || b.Cursor != p.After || b.After != p.After || !slices.Equal(b.Kinds, kinds) {
		return nil, protocol.E("invalid_state", "subscription binding differs from the published version or request")
	}
	for _, d := range pkg.Plugin.Events {
		if d.Ref == p.DefinitionRef && d.Direction == b.Direction && d.Name == b.Name {
			if b.Direction == "plugin" && (b.After != 0 || len(b.Kinds) > 0) {
				return nil, protocol.E("invalid_params", "plugin source starts at zero without Core filters")
			}
			catalog, err := plugin.RestoreCatalog(m, *pkg.Plugin)
			if err != nil {
				return nil, err
			}
			return catalog, catalog.ValidateSubscription(plugin.EventSubscribe{OperationID: b.OperationID, SubscriptionID: b.SubscriptionID, DefinitionRef: b.DefinitionRef, Scope: b.Scope, After: b.After, Kinds: b.Kinds})
		}
	}
	return nil, protocol.E("stale_reference", "event definition differs from publication")
}

func (s *Store) PriorPluginSubscription(ctx context.Context, req protocol.Request) (*PluginSubscriptionRecord, error) {
	p, err := subscriptionInput(req)
	if err != nil {
		return nil, err
	}
	_, found, err := s.AcceptedRequest(ctx, req)
	if err != nil || !found {
		return nil, err
	}
	r, err := s.PluginSubscription(ctx, subscriptionTarget(p, req))
	if err == nil && r == nil {
		err = protocol.E("invalid_state", "accepted subscription has no durable record")
	}
	return r, err
}

func (s *Store) PluginSubscriptionScope(ctx context.Context, req protocol.Request) (plugin.Scope, error) {
	p, err := subscriptionInput(req)
	if err != nil {
		return plugin.Scope{}, err
	}
	if err := pluginAgentDispatchable(ctx, s.db, p.AgentID); err != nil {
		return plugin.Scope{}, err
	}
	scope, _, err := pluginCommandScope(ctx, s.db, subscriptionSourceInput(p))
	return scope, err
}

func (s *Store) BeginPluginSubscription(ctx context.Context, i PluginSubscriptionIntent, owner string) (*PluginSubscriptionRecord, bool, error) {
	p, err := subscriptionInput(i.Request)
	if err != nil {
		return nil, false, err
	}
	if !pluginID(owner, true) {
		return nil, false, protocol.E("invalid_params", "Core owner is required")
	}
	fresh := false
	_, err = s.accept(ctx, i.Request, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := pluginAgentDispatchable(ctx, tx, p.AgentID); err != nil {
			return protocol.Accepted{}, err
		}
		scope, source, err := pluginCommandScope(ctx, tx, subscriptionSourceInput(p))
		if err != nil {
			return protocol.Accepted{}, err
		}
		if scope != i.Binding.Scope {
			return protocol.Accepted{}, protocol.E("selection_changed", "subscription scope changed before acceptance")
		}
		if scope.TaskID != "" {
			if err := activeTask(ctx, tx, scope.AgentID, scope.TaskID); err != nil {
				return protocol.Accepted{}, err
			}
		}
		i.Source = source
		if _, err := publishedSubscription(ctx, tx, i); err != nil {
			return protocol.Accepted{}, err
		}
		var latest int64
		if err := tx.QueryRowContext(ctx, `SELECT seq FROM agents WHERE id=?`, p.AgentID).Scan(&latest); err != nil {
			return protocol.Accepted{}, err
		}
		if i.Binding.Direction == "core" && p.After > latest {
			return protocol.Accepted{}, protocol.E("cursor_gap", "subscription cursor is ahead of Core history")
		}
		raw, err := json.Marshal(i)
		if err != nil || len(raw) > maxPluginIntentBytes {
			return protocol.Accepted{}, protocol.E("invalid_params", "subscription intent exceeds its retention budget")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_subscriptions(subscription_id,agent_id,owner,intent,digest) VALUES(?,?,?,?,?)`, i.Request.OperationID, p.AgentID, owner, string(raw), pluginDigest(raw)); err != nil {
			return protocol.Accepted{}, err
		}
		r := &PluginSubscriptionRecord{Intent: i, Owner: owner}
		if err := saveSubscriptionHead(ctx, tx, r, pluginSubscriptionHead{State: "opening", Cursor: p.After}, "opening"); err != nil {
			return protocol.Accepted{}, err
		}
		fresh = true
		return protocol.Accepted{AgentID: p.AgentID, TaskID: scope.TaskID}, nil
	})
	if err != nil {
		return nil, false, err
	}
	r, err := s.PluginSubscription(ctx, subscriptionTarget(p, i.Request))
	if err == nil && r == nil {
		err = protocol.E("invalid_state", "accepted subscription has no durable record")
	}
	return r, fresh && err == nil, err
}

func saveSubscriptionHead(ctx context.Context, tx *sql.Tx, r *PluginSubscriptionRecord, h pluginSubscriptionHead, event string) error {
	if r.revision != 0 && reflect.DeepEqual(r.head, h) {
		return nil
	}
	// Reserve entries for a close request, an already in-flight page/ack, and
	// closure. Retention exhaustion must still permit a confirmed close.
	if r.revision >= MaxPluginSubscriptionChanges && h.CloseOperationID == "" && subscriptionActive(h.State) || r.revision >= MaxPluginSubscriptionChanges+3 {
		return protocol.E("resource_limit", "subscription history limit reached; close it and retain existing records")
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_subscription_history(subscription_id,sequence,payload,digest) VALUES(?,?,?,?)`, r.Intent.Binding.SubscriptionID, r.revision+1, string(raw), pluginDigest(raw)); err != nil {
		return err
	}
	r.head, r.revision, r.Receipt = h, r.revision+1, subscriptionReceipt(r.Intent, h)
	if event != "" {
		_, err = appendEvent(ctx, tx, r.Receipt.AgentID, r.Receipt.Scope.TaskID, "plugin.subscription."+event, r.Receipt)
	}
	return err
}
