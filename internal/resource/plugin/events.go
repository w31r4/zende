package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sync"
	"time"

	"zende.local/harness/internal/protocol"
)

const MaxEventSubscriptions = 32
const MaxEventBatch = 64
const MaxEventBytes = 256 << 10
const MaxEventBatchBytes = 4 << 20
const MaxEventWaitMS = 1000
const EventRequestTimeout = 5 * time.Second

var eventKind = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)

type EventSubscribe struct {
	OperationID    string   `json:"operation_id"`
	SubscriptionID string   `json:"subscription_id"`
	DefinitionRef  string   `json:"definition_ref"`
	Scope          Scope    `json:"scope"`
	After          int64    `json:"after"`
	Kinds          []string `json:"kinds,omitempty"`
}

type EventOpen struct {
	EventSubscribe
	Name      string `json:"name"`
	Direction string `json:"direction"`
}

type EventAck struct {
	SubscriptionID string `json:"subscription_id"`
	Cursor         int64  `json:"cursor"`
	State          string `json:"state"`
}

// PluginEvent is plugin data. Its identity is (subscription_id, sequence), not
// a Core event's (agent_id, seq); Value never drives Core state transitions.
type PluginEvent struct {
	Sequence int64           `json:"sequence"`
	Kind     string          `json:"kind"`
	Value    json.RawMessage `json:"value"`
}

type PluginEventPage struct {
	SubscriptionID string        `json:"subscription_id"`
	After          int64         `json:"after"`
	Next           int64         `json:"next"`
	Events         []PluginEvent `json:"events"`
	Closed         bool          `json:"closed"`
}

type EventRead struct {
	SubscriptionID string `json:"subscription_id"`
	After          int64  `json:"after"`
	Limit          int    `json:"limit"`
	WaitMS         int    `json:"wait_ms"`
}

type CoreEventBatch struct {
	SubscriptionID string           `json:"subscription_id"`
	After          int64            `json:"after"`
	Through        int64            `json:"through"`
	Events         []protocol.Event `json:"events"`
}

type EventClose struct {
	SubscriptionID string `json:"subscription_id"`
	OperationID    string `json:"operation_id"`
	Cursor         int64  `json:"cursor"`
}

type EventSubscriptionInfo struct {
	CallBinding
	SubscriptionID string   `json:"subscription_id"`
	Direction      string   `json:"direction"`
	Name           string   `json:"name"`
	Scope          Scope    `json:"scope"`
	Kinds          []string `json:"kinds,omitempty"`
	After          int64    `json:"after"`
	Cursor         int64    `json:"cursor"`
	State          string   `json:"state"`
}

// The Core owns persistence and the instance lease. This object only owns a
// bound wire subscription; each exchange is short and serialized per subscription.
// Polling plugin events leaves the stdout reader free of unbounded push queues.
type EventSubscription struct {
	instance *Instance
	entry    eventEntry
	gate     chan struct{}
	mu       sync.Mutex
	info     EventSubscriptionInfo
}

func (s *EventSubscription) Info() EventSubscriptionInfo {
	s.mu.Lock()
	info := s.info
	info.Kinds = slices.Clone(info.Kinds)
	s.mu.Unlock()
	// A dead transport cannot confirm closure of its subscriptions. Keep a
	// verified terminal state, but never report an orphaned subscription open.
	if info.State != "closed" && info.State != "rejected" && s.instance.Info().State != "ready" {
		info.State = "unknown"
	}
	return info
}

func (s *EventSubscription) set(state string, cursor int64) {
	s.mu.Lock()
	s.info.State, s.info.Cursor = state, cursor
	s.mu.Unlock()
	if state == "closed" || state == "rejected" {
		s.instance.mu.Lock()
		delete(s.instance.subscriptions, s.info.SubscriptionID)
		s.instance.mu.Unlock()
	}
}

func (s *EventSubscription) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *EventSubscription) invalid(message string) error {
	info := s.Info()
	s.set("unknown", info.Cursor)
	err := protocol.E("plugin_protocol", message)
	s.instance.stop(err)
	return err
}

func (i *Instance) OpenEvents(ctx context.Context, p EventSubscribe) (*EventSubscription, error) {
	s, err := i.PrepareEvents(p)
	if err != nil {
		return nil, err
	}
	return s, s.Open(ctx)
}

// PrepareEvents validates and binds without sending RPC or reserving capacity.
// The Core can durably accept this exact binding before Open dispatches it.
func (i *Instance) PrepareEvents(p EventSubscribe) (*EventSubscription, error) {
	if !boundedID(p.OperationID, 128) || !boundedID(p.SubscriptionID, 128) || p.Scope.AgentID == "" || p.After < 0 || len(p.Kinds) > 32 {
		return nil, protocol.E("invalid_params", "event subscription requires operation, subscription, Agent and a nonnegative cursor")
	}
	var err error
	p.Scope, err = i.validateScope(p.Scope)
	if err != nil {
		return nil, err
	}
	p.Kinds = slices.Clone(p.Kinds)
	slices.Sort(p.Kinds)
	for n, kind := range p.Kinds {
		if !eventKind.MatchString(kind) || n > 0 && kind == p.Kinds[n-1] {
			return nil, protocol.E("invalid_params", "Core event kinds must be unique names of 1–128 bytes")
		}
	}
	i.mu.Lock()
	if i.info.State != "ready" || i.catalog == nil {
		i.mu.Unlock()
		return nil, protocol.E("unavailable", "plugin event catalog is unavailable")
	}
	var entry *eventEntry
	for _, e := range i.catalog.events {
		if e.definition.Ref == p.DefinitionRef {
			entry = &e
			break
		}
	}
	if entry == nil {
		i.mu.Unlock()
		return nil, protocol.E("stale_reference", "plugin event definition changed; inspect the current catalog")
	}
	if entry.definition.Direction == "plugin" && (p.After != 0 || len(p.Kinds) != 0) {
		i.mu.Unlock()
		return nil, protocol.E("invalid_params", "new plugin event subscriptions start at zero without Core filters")
	}
	s := &EventSubscription{instance: i, entry: *entry, gate: make(chan struct{}, 1), info: EventSubscriptionInfo{
		CallBinding:    CallBinding{PluginID: i.info.ID, PluginVersion: i.info.Version, ManifestVersion: i.info.ManifestVersion, InstanceID: i.info.InstanceID, CatalogVersion: i.catalog.version, DefinitionRef: p.DefinitionRef, OperationID: p.OperationID},
		SubscriptionID: p.SubscriptionID, Direction: entry.definition.Direction, Name: entry.definition.Name,
		Scope: p.Scope, Kinds: p.Kinds, After: p.After, Cursor: p.After, State: "prepared",
	}}
	i.mu.Unlock()
	return s, nil
}

func (s *EventSubscription) Open(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	info := s.Info()
	if info.State != "prepared" {
		return protocol.E("operation_conflict", "subscription was already opened or its instance is unavailable")
	}
	i := s.instance
	i.mu.Lock()
	if i.info.State != "ready" {
		i.mu.Unlock()
		return protocol.E("unavailable", "plugin event instance is unavailable")
	}
	if i.subscriptions[info.SubscriptionID] != nil {
		i.mu.Unlock()
		return protocol.E("operation_conflict", "subscription already exists on this instance; inspect the original operation")
	}
	if len(i.subscriptions) >= MaxEventSubscriptions {
		i.mu.Unlock()
		return protocol.E("resource_limit", "plugin permits at most 32 open event subscriptions")
	}
	if i.subscriptions == nil {
		i.subscriptions = map[string]*EventSubscription{}
	}
	i.subscriptions[info.SubscriptionID] = s
	i.mu.Unlock()
	s.set("opening", info.After)
	p := EventSubscribe{OperationID: info.OperationID, SubscriptionID: info.SubscriptionID, DefinitionRef: info.DefinitionRef, Scope: info.Scope, After: info.After, Kinds: info.Kinds}
	raw, sent, err := i.eventExchange(ctx, "plugin.events.open", EventOpen{p, info.Name, info.Direction})
	if err != nil {
		if sent {
			s.set("unknown", p.After)
		} else {
			s.set("rejected", p.After)
		}
		return err
	}
	if !eventAck(raw, p.SubscriptionID, p.After, "open") {
		return s.invalid("plugin event open acknowledgement does not match the subscription")
	}
	s.set("open", p.After)
	return nil
}

func eventAck(raw []byte, id string, cursor int64, state string) bool {
	var ack EventAck
	return requiredEventJSON(raw, &ack, "subscription_id", "cursor", "state") == nil && ack.SubscriptionID == id && ack.Cursor == cursor && ack.State == state
}

func requiredEventJSON(raw []byte, result any, required ...string) error {
	if len(raw) > MaxEventBatchBytes || protocol.StrictDecode(raw, result) != nil {
		return protocol.E("plugin_protocol", "invalid event response")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return protocol.E("plugin_protocol", "invalid event response")
	}
	for _, key := range required {
		if len(fields[key]) == 0 || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return protocol.E("plugin_protocol", "event response omits a required field")
		}
	}
	return nil
}

// Read is non-consuming. Only a later After acknowledges the host's persisted
// prefix; retrying an earlier cursor must return the same records or a visible
// gap error. The host must persist a page before advancing its durable cursor.
func (s *EventSubscription) Read(ctx context.Context, after int64, limit, waitMS int) (PluginEventPage, error) {
	var page PluginEventPage
	if err := s.lock(ctx); err != nil {
		return page, err
	}
	defer func() { <-s.gate }()
	info := s.Info()
	if info.State != "open" || info.Direction != "plugin" || after < info.After || after > info.Cursor || limit < 1 || limit > MaxEventBatch || waitMS < 0 || waitMS > MaxEventWaitMS {
		return page, protocol.E("invalid_params", "read requires an open plugin source and a previously observed cursor within event limits")
	}
	raw, sent, err := s.instance.eventExchange(ctx, "plugin.events.read", EventRead{info.SubscriptionID, after, limit, waitMS})
	if err != nil {
		var failure *protocol.Error
		if sent && errors.As(err, &failure) && failure.Data.Kind == "transport_unknown" {
			s.set("unknown", info.Cursor)
		}
		return page, err
	}
	if requiredEventJSON(raw, &page, "subscription_id", "after", "next", "events", "closed") != nil || page.SubscriptionID != info.SubscriptionID || page.After != after || len(page.Events) > limit || page.Next < after || page.Next-after != int64(len(page.Events)) {
		return PluginEventPage{}, s.invalid("plugin event page has a missing range or wrong subscription")
	}
	for n, event := range page.Events {
		if event.Sequence != after+int64(n)+1 || !eventKind.MatchString(event.Kind) {
			return PluginEventPage{}, s.invalid("plugin event identity is invalid or out of order")
		}
		value, err := decodeValue(event.Value, MaxEventBytes)
		if err != nil || s.entry.schema.Validate(value) != nil {
			return PluginEventPage{}, s.invalid("plugin event value exceeds its budget or differs from the published schema")
		}
	}
	if page.Closed {
		s.set("closed", max(info.Cursor, page.Next))
	} else {
		s.set("open", max(info.Cursor, page.Next))
	}
	return page, nil
}

// Deliver retains original Core event identities. Through may cover filtered
// events omitted from this batch; it never rewrites their Agent or sequence.
func (s *EventSubscription) Deliver(ctx context.Context, batch CoreEventBatch) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	info := s.Info()
	if info.State != "open" || info.Direction != "core" || batch.SubscriptionID != info.SubscriptionID || batch.After != info.Cursor || batch.Through < batch.After || len(batch.Events) > MaxEventBatch {
		return protocol.E("invalid_params", "delivery requires an open Core listener and its current cursor")
	}
	previous := batch.After
	if batch.Events == nil {
		batch.Events = []protocol.Event{}
	}
	for _, event := range batch.Events {
		if event.Version != protocol.Version || event.AgentID != info.Scope.AgentID || event.TaskID != "" && !boundedID(event.TaskID, 128) || event.Seq <= previous || event.Seq > batch.Through || !eventKind.MatchString(event.Kind) || len(info.Kinds) > 0 && !slices.Contains(info.Kinds, event.Kind) {
			return protocol.E("invalid_params", "Core event does not match subscription scope, filter or sequence")
		}
		if _, err := decodeValue(event.Payload, MaxEventBatchBytes); err != nil {
			return err
		}
		previous = event.Seq
	}
	raw, err := json.Marshal(batch)
	if err != nil || len(raw) > MaxEventBatchBytes {
		return protocol.E("resource_limit", "Core event batch exceeds 4 MiB; retain the cursor and use a smaller batch")
	}
	raw, sent, err := s.instance.eventExchange(ctx, "plugin.events.deliver", batch)
	if err != nil {
		if sent {
			s.set("unknown", info.Cursor)
		}
		return err
	}
	if !eventAck(raw, info.SubscriptionID, batch.Through, "open") {
		return s.invalid("plugin did not acknowledge the exact Core event batch")
	}
	s.set("open", batch.Through)
	return nil
}

func (s *EventSubscription) Close(ctx context.Context, operationID string) error {
	if !boundedID(operationID, 128) {
		return protocol.E("invalid_params", "event close requires a bounded operation ID")
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	info := s.Info()
	if info.State == "closed" {
		return nil
	}
	if info.State != "open" {
		return protocol.E("transport_unknown", "event subscription is not open; inspect its original state")
	}
	s.set("closing", info.Cursor)
	raw, sent, err := s.instance.eventExchange(ctx, "plugin.events.close", EventClose{info.SubscriptionID, operationID, info.Cursor})
	if err != nil {
		if sent {
			s.set("unknown", info.Cursor)
		} else {
			s.set("open", info.Cursor)
		}
		return err
	}
	if !eventAck(raw, info.SubscriptionID, info.Cursor, "closed") {
		return s.invalid("plugin did not confirm subscription closure")
	}
	s.set("closed", info.Cursor)
	return nil
}

// After dispatch, drain the bounded exchange even if the caller disconnects:
// cancellation cannot manufacture a closed subscription or discard a valid page.
// There is no automatic resend. Unconfirmed mutating operations are unknown.
func (i *Instance) eventExchange(ctx context.Context, method string, params any) (json.RawMessage, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	p, sent, err := i.startRequest(ctx, method, params, "events")
	unknown := func() (json.RawMessage, bool, error) {
		i.stop(protocol.E("plugin_protocol", "event operation ended without a verified response"))
		return nil, true, protocol.E("transport_unknown", "plugin event operation is unconfirmed; it was not retried")
	}
	if err != nil {
		if sent {
			return unknown()
		}
		return nil, false, err
	}
	defer i.forget(p)
	timer := time.NewTimer(EventRequestTimeout)
	defer timer.Stop()
	var reply response
	select {
	case reply = <-p.ch:
	default:
		select {
		case reply = <-p.ch:
		case <-timer.C:
			select {
			case reply = <-p.ch:
			default:
				return unknown()
			}
		case <-i.done:
			select {
			case reply = <-p.ch:
			default:
				return unknown()
			}
		}
	}
	if len(reply.Error) != 0 {
		if method == "plugin.events.read" {
			return nil, true, protocol.E("event_unavailable", "plugin event range is unavailable; inspect the subscription without advancing its cursor")
		}
		return unknown()
	}
	return reply.Result, true, nil
}
