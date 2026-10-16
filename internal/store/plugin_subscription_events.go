package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func pluginEventJSON(value any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

func (s *Store) AppendPluginEvents(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, page plugin.PluginEventPage) error {
	raw, err := pluginEventJSON(page)
	if err != nil || len(raw) > plugin.MaxEventBatchBytes || page.SubscriptionID != target.SubscriptionID || page.After < 0 || page.Next < page.After || page.Next-page.After != int64(len(page.Events)) || len(page.Events) > plugin.MaxEventBatch || page.Next > MaxPluginSubscriptionEvents {
		return protocol.E("invalid_params", "plugin event page identity, range or budget is invalid")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if r.Receipt.Direction != "plugin" || page.After > r.head.Cursor || page.Closed && page.Next < r.head.Cursor {
			return protocol.E("operation_conflict", "plugin event page does not extend this subscription's cursor")
		}
		catalog, err := publishedSubscription(ctx, tx, r.Intent)
		if err != nil {
			return err
		}
		h := r.head
		for n, event := range page.Events {
			if event.Sequence != page.After+int64(n)+1 {
				return protocol.E("invalid_params", "plugin event page is not contiguous")
			}
			if err := catalog.ValidateEvent(r.Receipt.DefinitionRef, event); err != nil {
				return err
			}
			data, err := pluginEventJSON(event)
			if err != nil || len(data) > MaxPluginEventRecordBytes {
				return protocol.E("resource_limit", "plugin event record exceeds its byte limit")
			}
			if event.Sequence <= r.head.Cursor {
				var saved, hash string
				if err := tx.QueryRowContext(ctx, `SELECT payload,digest FROM plugin_subscription_events WHERE subscription_id=? AND sequence=?`, target.SubscriptionID, event.Sequence).Scan(&saved, &hash); err != nil {
					return protocol.E("invalid_state", "retained plugin event range is missing")
				}
				if saved != string(data) || hash != pluginDigest(data) {
					return protocol.E("operation_conflict", "plugin event replay differs from its original record")
				}
				continue
			}
			if (h.State != "open" && h.State != "closing") || event.Sequence != h.Cursor+1 {
				return protocol.E("operation_conflict", "plugin event arrived outside an open ordered subscription")
			}
			if h.ReceivedBytes+int64(len(data)) > MaxPluginSubscriptionBytes {
				return protocol.E("resource_limit", "plugin event retention is full; existing records remain readable")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_subscription_events(subscription_id,sequence,payload,digest) VALUES(?,?,?,?)`, target.SubscriptionID, event.Sequence, string(data), pluginDigest(data)); err != nil {
				return err
			}
			h.Cursor, h.ReceivedBytes = event.Sequence, h.ReceivedBytes+int64(len(data))
		}
		event := "received"
		if page.Closed {
			if h.State != "open" && h.State != "closing" && h.State != "closed" {
				return protocol.E("operation_conflict", "plugin source cannot close a terminal subscription")
			}
			h.State, event = "closed", "closed"
		}
		return saveSubscriptionHead(ctx, tx, r, h, event)
	})
}

type PluginEventsPage struct {
	Subscription PluginSubscriptionReceipt `json:"subscription"`
	Events       []plugin.PluginEvent      `json:"events"`
	Next         int64                     `json:"next"`
	More         bool                      `json:"more"`
}

func (s *Store) ReadPluginEvents(ctx context.Context, p protocol.PluginEventsRead) (PluginEventsPage, error) {
	out := PluginEventsPage{Events: []plugin.PluginEvent{}, Next: p.After}
	if !pluginID(p.AgentID, true) || !pluginID(p.SubscriptionID, true) || p.After < 0 || p.Limit < 0 || p.Limit > plugin.MaxEventBatch {
		return out, protocol.E("invalid_params", "plugin events accepts Agent, subscription, nonnegative cursor and limit 1–64")
	}
	if p.Limit == 0 {
		p.Limit = 16
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := subscriptionRecord(ctx, tx, p.PluginSubscriptionTarget)
		if err != nil {
			return err
		}
		if r == nil {
			return protocol.E("not_found", "plugin subscription not found for this Agent")
		}
		if r.Receipt.Direction != "plugin" {
			return protocol.E("invalid_params", "Core listeners retain their source in Agent history; this reader serves plugin events")
		}
		if p.After > r.head.Cursor {
			return protocol.E("cursor_gap", "plugin event cursor is ahead of retained history")
		}
		catalog, err := publishedSubscription(ctx, tx, r.Intent)
		if err != nil {
			return err
		}
		out.Subscription = r.Receipt
		rows, err := tx.QueryContext(ctx, `SELECT sequence,payload,digest FROM plugin_subscription_events WHERE subscription_id=? AND sequence>? ORDER BY sequence LIMIT ?`, p.SubscriptionID, p.After, p.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		receiptJSON, _ := json.Marshal(r.Receipt)
		total := len(receiptJSON) + 512
		budgetCut := false
		for rows.Next() {
			var seq int64
			var raw, hash string
			var event plugin.PluginEvent
			if err := rows.Scan(&seq, &raw, &hash); err != nil {
				return err
			}
			if len(raw) > MaxPluginEventRecordBytes || hash != pluginDigest([]byte(raw)) || protocol.StrictDecode([]byte(raw), &event) != nil || seq != out.Next+1 || event.Sequence != seq || seq > r.head.Cursor || catalog.ValidateEvent(r.Receipt.DefinitionRef, event) != nil {
				return protocol.E("invalid_state", "retained plugin event failed integrity verification")
			}
			// JSON encoding on IPC may escape '<', '>' and '&'. Account for
			// actual wire size so a valid stored page always fits the socket.
			wire, _ := json.Marshal(event)
			if total+len(wire)+1 > protocol.MaxFrameBytes-4096 {
				budgetCut = true
				break
			}
			out.Events, out.Next = append(out.Events, event), seq
			total += len(wire) + 1
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !budgetCut && len(out.Events) < p.Limit && out.Next < r.head.Cursor {
			return protocol.E("cursor_gap", "retained plugin event range is missing")
		}
		if len(out.Events) == 0 && budgetCut {
			return protocol.E("output_limit", "plugin event and subscription metadata exceed the IPC response budget")
		}
		out.More = out.Next < r.head.Cursor
		return nil
	})
	if err != nil {
		return PluginEventsPage{}, err
	}
	return out, nil
}

// BeginPluginEventDelivery saves the exact batch identity before RPC. Existing
// pending deliveries are never handed out a second time, even after a restart.
func (s *Store) BeginPluginEventDelivery(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string) (*plugin.CoreEventBatch, error) {
	var batch *plugin.CoreEventBatch
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if r.Receipt.Direction != "core" || r.head.State != "open" || r.head.CloseOperationID != "" {
			return protocol.E("operation_conflict", "Core event listener is not open for new deliveries")
		}
		if r.head.Delivery.Pending() {
			return protocol.E("operation_unknown", "Core event batch was already dispatched; inspect its original receipt")
		}
		b := plugin.CoreEventBatch{SubscriptionID: target.SubscriptionID, After: r.head.Cursor, Through: r.head.Cursor, Events: []protocol.Event{}}
		rows, err := tx.QueryContext(ctx, `SELECT seq,kind,task_id,payload FROM events WHERE agent_id=? AND seq>? ORDER BY seq LIMIT ?`, target.AgentID, b.After, plugin.MaxEventBatch)
		if err != nil {
			return err
		}
		total := 512
		for rows.Next() {
			e := protocol.Event{Version: protocol.Version, AgentID: target.AgentID}
			var payload string
			if err := rows.Scan(&e.Seq, &e.Kind, &e.TaskID, &payload); err != nil {
				rows.Close()
				return err
			}
			if e.Seq != b.Through+1 {
				rows.Close()
				return protocol.E("cursor_gap", "Core event range is missing")
			}
			if len(r.Receipt.Kinds) == 0 || slices.Contains(r.Receipt.Kinds, e.Kind) {
				e.Payload = json.RawMessage(payload)
				raw, err := json.Marshal(e)
				if err != nil || len(raw)+512 > plugin.MaxEventBatchBytes {
					rows.Close()
					return protocol.E("resource_limit", "Core event exceeds plugin delivery budget; original history remains readable")
				}
				if total+len(raw)+1 > plugin.MaxEventBatchBytes {
					break
				}
				b.Events = append(b.Events, e)
				total += len(raw) + 1
			}
			b.Through = e.Seq
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if b.Through == b.After {
			var latest int64
			if err := tx.QueryRowContext(ctx, `SELECT seq FROM agents WHERE id=?`, target.AgentID).Scan(&latest); err != nil {
				return err
			}
			if latest != b.After {
				return protocol.E("cursor_gap", "Core event cursor does not match retained history")
			}
			return nil
		}
		raw, _ := json.Marshal(b)
		h := r.head
		h.Delivery = &PluginEventDelivery{After: b.After, Through: b.Through, SHA256: pluginDigest(raw)}
		if err := saveSubscriptionHead(ctx, tx, r, h, ""); err != nil {
			return err
		}
		batch = &b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

func (s *Store) ConfirmPluginEventDelivery(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, batch plugin.CoreEventBatch) error {
	return s.finishPluginEventDelivery(ctx, target, owner, batch, true)
}

// Only a transport rejection before any byte was sent permits another attempt.
// The rejected intent remains in history; cursor and plugin effects stay put.
func (s *Store) RejectPluginEventDelivery(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, batch plugin.CoreEventBatch) error {
	return s.finishPluginEventDelivery(ctx, target, owner, batch, false)
}

func (s *Store) finishPluginEventDelivery(ctx context.Context, target protocol.PluginSubscriptionTarget, owner string, batch plugin.CoreEventBatch, confirmed bool) error {
	raw, err := json.Marshal(batch)
	if err != nil || len(raw) > plugin.MaxEventBatchBytes || batch.SubscriptionID != target.SubscriptionID {
		return protocol.E("invalid_params", "invalid Core event delivery acknowledgement")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedSubscription(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		d := r.head.Delivery
		if d == nil || d.After != batch.After || d.Through != batch.Through || d.SHA256 != pluginDigest(raw) {
			return protocol.E("operation_conflict", "delivery acknowledgement differs from its original batch")
		}
		if !d.Pending() {
			if d.Confirmed == confirmed {
				return nil
			}
			return protocol.E("operation_conflict", "delivery already has a different outcome")
		}
		if r.head.State != "open" && r.head.State != "closing" {
			return protocol.E("operation_conflict", "delivery cannot change a terminal subscription")
		}
		h := r.head
		outcome := *d
		outcome.Confirmed, outcome.NotDispatched = confirmed, !confirmed
		h.Delivery = &outcome
		if confirmed {
			h.Cursor = d.Through
		}
		return saveSubscriptionHead(ctx, tx, r, h, "")
	})
}
