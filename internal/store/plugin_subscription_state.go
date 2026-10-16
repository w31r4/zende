package store

import (
	"context"
	"database/sql"
	"reflect"
	"slices"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func sameSubscriptionBinding(a, b plugin.EventSubscriptionInfo) bool {
	a.State, a.Cursor = b.State, b.Cursor
	if !slices.Equal(a.Kinds, b.Kinds) {
		return false
	}
	a.Kinds = b.Kinds
	return reflect.DeepEqual(a, b)
}

func (s *Store) OpenPluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, info plugin.EventSubscriptionInfo) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if !sameSubscriptionBinding(info, r.Intent.Binding) || info.State != "open" || info.Cursor != r.Intent.Binding.After {
			return protocol.E("operation_conflict", "subscription open acknowledgement differs from the retained binding")
		}
		if r.head.State == "open" || r.head.State == "closing" {
			return nil
		}
		if r.head.State != "opening" {
			return protocol.E("operation_conflict", "subscription already has a terminal record")
		}
		h := r.head
		h.State = "open"
		if h.CloseOperationID != "" {
			h.State = "closing"
		}
		return saveSubscriptionHead(ctx, tx, r, h, "opened")
	})
}

func (s *Store) ClosePluginSubscription(ctx context.Context, req protocol.Request) (protocol.Accepted, error) {
	var target protocol.PluginSubscriptionTarget
	if req.Validate() != nil || req.Method != "plugin.events.close" || !pluginID(req.OperationID, true) || protocol.StrictDecode(req.Params, &target) != nil || !pluginID(target.AgentID, true) || !pluginID(target.SubscriptionID, true) {
		return protocol.Accepted{}, protocol.E("invalid_params", "close requires Agent, subscription and an independent request ID")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		r, err := subscriptionRecord(ctx, tx, target)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if r == nil {
			return protocol.Accepted{}, protocol.E("not_found", "plugin subscription not found for this Agent")
		}
		if err := requestSubscriptionClose(ctx, tx, r, req.OperationID, ""); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: target.AgentID, TaskID: r.Receipt.Scope.TaskID}, nil
	})
}

func requestSubscriptionClose(ctx context.Context, tx *sql.Tx, r *PluginSubscriptionRecord, operation, reason string) error {
	if !subscriptionActive(r.head.State) || r.head.CloseOperationID != "" {
		return nil
	}
	h := r.head
	h.CloseOperationID, h.Reason = operation, reason
	if h.State == "open" {
		h.State = "closing"
	}
	return saveSubscriptionHead(ctx, tx, r, h, "closing")
}

// Host shutdown, explicit tree stop and retention exhaustion use the same
// durable close intent. These reasons never claim that external effects rolled back.
func (s *Store) RequestPluginSubscriptionClose(ctx context.Context, target protocol.PluginSubscriptionTarget, owner, reason string) error {
	if reason != "core_shutdown" && reason != "tree_stop" && reason != "resource_limit" {
		return protocol.E("invalid_params", "invalid subscription close reason")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		return requestSubscriptionClose(ctx, tx, r, protocol.NewID("close"), reason)
	})
}

func (s *Store) ConfirmPluginSubscriptionClosed(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, info plugin.EventSubscriptionInfo) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if !sameSubscriptionBinding(info, r.Intent.Binding) || info.State != "closed" || info.Cursor != r.head.Cursor || r.head.Delivery.Pending() {
			return protocol.E("operation_conflict", "subscription closure differs from its binding or durable cursor")
		}
		if r.head.State == "closed" {
			return nil
		}
		if r.head.State != "closing" || r.head.CloseOperationID == "" {
			return protocol.E("operation_conflict", "subscription close was not requested or is already terminal")
		}
		h := r.head
		h.State = "closed"
		return saveSubscriptionHead(ctx, tx, r, h, "closed")
	})
}

func (s *Store) FailPluginSubscription(ctx context.Context, target protocol.PluginSubscriptionTarget, owner, state, reason string) error {
	if state != "unknown" && state != "not_dispatched" || reason != "transport_unknown" && reason != "event_unavailable" && reason != "resource_limit" && reason != "persistence_failed" && reason != "core_restarted" && reason != "not_dispatched" {
		return protocol.E("invalid_params", "invalid subscription failure state or reason")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if !subscriptionActive(r.head.State) {
			if r.head.State == state && r.head.Reason == reason {
				return nil
			}
			return protocol.E("operation_conflict", "subscription already has a different terminal record")
		}
		if state == "not_dispatched" && (r.head.State != "opening" || r.head.Cursor != r.Intent.Binding.After || r.head.Delivery != nil) {
			return protocol.E("invalid_state", "active subscription cannot become not dispatched")
		}
		h := r.head
		h.State, h.Reason = state, reason
		return saveSubscriptionHead(ctx, tx, r, h, state)
	})
}

// A replacement Core retains the original owner, cursor and uncertain delivery.
// It can inspect history but cannot silently reopen or redeliver the operation.
func (s *Store) InterruptPluginSubscriptions(ctx context.Context, owner string) error {
	if !pluginID(owner, true) {
		return protocol.E("invalid_params", "Core owner is required")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		targets, err := subscriptionTargets(ctx, tx, `owner<>? AND json_extract(payload,'$.state') IN ('opening','open','closing')`, owner)
		if err != nil {
			return err
		}
		for _, target := range targets {
			r, err := subscriptionRecord(ctx, tx, target)
			if err != nil {
				return err
			}
			h := r.head
			h.State, h.Reason = "unknown", "core_restarted"
			if err := saveSubscriptionHead(ctx, tx, r, h, "unknown"); err != nil {
				return err
			}
		}
		return nil
	})
}

func subscriptionTargets(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]protocol.PluginSubscriptionTarget, error) {
	rows, err := tx.QueryContext(ctx, `SELECT agent_id,subscription_id FROM plugin_subscription_heads WHERE `+where+` ORDER BY subscription_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []protocol.PluginSubscriptionTarget
	for rows.Next() {
		var target protocol.PluginSubscriptionTarget
		if err := rows.Scan(&target.AgentID, &target.SubscriptionID); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func closeAgentPluginSubscriptions(ctx context.Context, tx *sql.Tx, agent string) error {
	targets, err := subscriptionTargets(ctx, tx, `agent_id=? AND json_extract(payload,'$.state') IN ('opening','open','closing')`, agent)
	if err != nil {
		return err
	}
	for _, target := range targets {
		r, err := subscriptionRecord(ctx, tx, target)
		if err != nil {
			return err
		}
		if err := requestSubscriptionClose(ctx, tx, r, protocol.NewID("close"), "tree_stop"); err != nil {
			return err
		}
	}
	return nil
}

// Each admitted read fits even if every event uses its full value budget.
// Once exhausted, the worker closes instead of accepting then losing a page.
func (r *PluginSubscriptionRecord) PluginEventReadLimit() int {
	if r.Receipt.Direction != "plugin" || r.revision >= MaxPluginSubscriptionChanges-1 {
		return 0
	}
	return int(min(int64(plugin.MaxEventBatch), MaxPluginSubscriptionEvents-r.head.Cursor, (MaxPluginSubscriptionBytes-r.head.ReceivedBytes)/MaxPluginEventRecordBytes))
}

func (r *PluginSubscriptionRecord) SubscriptionHistoryFull() bool {
	return r.revision >= MaxPluginSubscriptionChanges-2
}
