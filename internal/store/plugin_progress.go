package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func pendingPlugin(state string) bool { return state == "dispatched" || state == "cancelling" }

func ownedPlugin(ctx context.Context, q rowQuerier, target PluginCallTarget, owner string) (*PluginRecord, error) {
	r, err := pluginRecord(ctx, q, target)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, protocol.E("not_found", "plugin call not found for this Agent")
	}
	if r.Owner != owner {
		return nil, protocol.E("operation_conflict", "plugin call belongs to a different Core instance")
	}
	return r, nil
}

// AppendPluginProgress acknowledges durable, ordered progress to Invoke.
// Replays must be identical; gaps, conflicts and exhausted budgets fail.
func (s *Store) AppendPluginProgress(ctx context.Context, target PluginCallTarget, owner string, p plugin.Progress) error {
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > plugin.MaxProgressBytes || !pluginID(p.RequestID, true) || p.OperationID != target.OperationID || p.Sequence <= 0 || p.Sequence > MaxPluginProgressEntries || !json.Valid(p.Value) || !utf8.Valid(p.Value) || protocol.StrictDecode(p.Value, new(json.RawMessage)) != nil {
		return protocol.E("invalid_params", "plugin progress identity, sequence or byte limit is invalid")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedPlugin(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if p.Sequence <= r.Receipt.ProgressSeq {
			var saved, hash string
			if err := tx.QueryRowContext(ctx, `SELECT payload,digest FROM plugin_progress WHERE operation_id=? AND sequence=?`, target.OperationID, p.Sequence).Scan(&saved, &hash); err != nil {
				return err
			}
			if saved != string(raw) || hash != pluginDigest(raw) {
				return protocol.E("operation_conflict", "plugin progress differs from its original record")
			}
			return nil
		}
		if !pendingPlugin(r.Receipt.State) || p.Sequence != r.Receipt.ProgressSeq+1 || r.Receipt.RequestID != "" && r.Receipt.RequestID != p.RequestID {
			return protocol.E("operation_conflict", "plugin progress is late, out of order or belongs to another request")
		}
		if r.Receipt.ProgressBytes+int64(len(raw)) > MaxPluginProgressBytes {
			return protocol.E("resource_limit", "plugin retained progress exceeds 8 MiB; original progress remains readable")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_progress(operation_id,sequence,payload,digest) VALUES(?,?,?,?)`, target.OperationID, p.Sequence, string(raw), pluginDigest(raw)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_calls SET request_id=?,progress_seq=?,progress_bytes=progress_bytes+? WHERE operation_id=?`, p.RequestID, p.Sequence, len(raw), target.OperationID); err != nil {
			return err
		}
		r.Receipt.RequestID, r.Receipt.ProgressSeq = p.RequestID, p.Sequence
		r.Receipt.ProgressBytes += int64(len(raw))
		_, err = appendEvent(ctx, tx, target.AgentID, r.Receipt.TaskID, "plugin.progress", r.Receipt)
		return err
	})
}

// The runtime must save cancellation before sending it. A cancellation request
// does not replace a verified terminal result or assert rollback of effects.
func (s *Store) MarkPluginCancelling(ctx context.Context, target PluginCallTarget, owner string) (*PluginRecord, error) {
	var r *PluginRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		r, err = ownedPlugin(ctx, tx, target, owner)
		if err != nil || r.Receipt.State != "dispatched" {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_calls SET state='cancelling' WHERE operation_id=?`, target.OperationID); err != nil {
			return err
		}
		r.Receipt.State = "cancelling"
		_, err = appendEvent(ctx, tx, target.AgentID, r.Receipt.TaskID, "plugin.cancelling", r.Receipt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, err
}

type PluginProgressPage struct {
	PluginCallTarget
	Items []plugin.Progress `json:"items"`
	Next  int64             `json:"next"`
	More  bool              `json:"more"`
}

func (s *Store) PluginProgress(ctx context.Context, target PluginCallTarget, after int64, limit int) (PluginProgressPage, error) {
	out := PluginProgressPage{PluginCallTarget: target, Items: []plugin.Progress{}, Next: after}
	if !pluginID(target.AgentID, true) || !pluginID(target.OperationID, true) || after < 0 || limit < 0 || limit > 32 {
		return out, protocol.E("invalid_params", "plugin progress accepts a nonnegative cursor and limit 1–32")
	}
	if limit == 0 {
		limit = 16
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := pluginRecord(ctx, tx, target)
		if err != nil {
			return err
		}
		if r == nil {
			return protocol.E("not_found", "plugin call not found for this Agent")
		}
		if after > r.Receipt.ProgressSeq {
			return protocol.E("cursor_gap", "plugin progress cursor is ahead of retained history")
		}
		rows, err := tx.QueryContext(ctx, `SELECT sequence,payload,digest FROM plugin_progress WHERE operation_id=? AND sequence>? ORDER BY sequence LIMIT ?`, target.OperationID, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sequence int64
			var raw, hash string
			var p plugin.Progress
			if err := rows.Scan(&sequence, &raw, &hash); err != nil {
				return err
			}
			if len(raw) > plugin.MaxProgressBytes || hash != pluginDigest([]byte(raw)) || protocol.StrictDecode([]byte(raw), &p) != nil || sequence != out.Next+1 || p.Sequence != sequence || p.OperationID != target.OperationID || p.RequestID != r.Receipt.RequestID {
				return protocol.E("invalid_state", "saved plugin progress failed integrity verification")
			}
			out.Items, out.Next = append(out.Items, p), sequence
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Items) < limit && out.Next != r.Receipt.ProgressSeq {
			return protocol.E("cursor_gap", "saved plugin progress has a gap")
		}
		out.More = out.Next < r.Receipt.ProgressSeq
		return nil
	})
	return out, err
}
