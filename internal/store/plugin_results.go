package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func (s *Store) FinishPluginCall(ctx context.Context, target PluginCallTarget, owner string, result plugin.CallResult) (*PluginRecord, error) {
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > maxPluginResultBytes || !pluginID(result.RequestID, false) {
		return nil, protocol.E("invalid_params", "plugin result exceeds its byte or request identity limit")
	}
	var r *PluginRecord
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		r, err = ownedPlugin(ctx, tx, target, owner)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result.CallBinding, r.Invocation.Binding) || r.Receipt.RequestID != "" && result.RequestID != r.Receipt.RequestID {
			return protocol.E("operation_conflict", "plugin result does not match its original instance and request")
		}
		if !pendingPlugin(r.Receipt.State) {
			var saved string
			if err := tx.QueryRowContext(ctx, `SELECT result FROM plugin_calls WHERE operation_id=?`, target.OperationID).Scan(&saved); err != nil {
				return err
			}
			if saved != string(raw) || r.Receipt.ResultSHA256 != pluginDigest(raw) {
				return protocol.E("operation_conflict", "plugin result already has a different terminal record")
			}
			return nil
		}
		switch result.State {
		case "completed", "failed", "cancelled":
			if result.RequestID == "" || result.Completion == nil || result.Completion.State != result.State {
				return protocol.E("invalid_state", "plugin outcome has no verified completion")
			}
			catalog, err := publishedPluginCall(ctx, tx, r.Invocation)
			if err != nil {
				return err
			}
			completion, err := catalog.ValidateCompletion(result.DefinitionRef, result.OperationID, result.Evidence)
			if err != nil || !reflect.DeepEqual(completion, *result.Completion) {
				return protocol.E("invalid_state", "plugin outcome differs from original schema or retained evidence")
			}
		case "not_dispatched":
			if r.Receipt.ProgressSeq != 0 || len(result.Evidence) != 0 || result.Completion != nil {
				return protocol.E("invalid_state", "unsent plugin call cannot have progress or a completion")
			}
		case "unknown":
			if result.Completion != nil {
				return protocol.E("invalid_state", "unknown plugin call cannot claim a verified completion")
			}
		default:
			return protocol.E("invalid_params", "invalid plugin terminal state")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_calls SET state=?,request_id=?,result=?,result_hash=? WHERE operation_id=?`, result.State, result.RequestID, string(raw), pluginDigest(raw), target.OperationID); err != nil {
			return err
		}
		r.Receipt.State, r.Receipt.RequestID = result.State, result.RequestID
		r.Receipt.ResultBytes, r.Receipt.ResultSHA256 = len(raw), pluginDigest(raw)
		r.Receipt.Error = pluginStateError(result.State)
		_, err = appendEvent(ctx, tx, target.AgentID, r.Receipt.TaskID, "plugin."+result.State, r.Receipt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, err
}

// Core replacement never grants dispatch for an accepted unfinished call.
func (s *Store) InterruptPluginCalls(ctx context.Context, owner string) error {
	if !pluginID(owner, true) {
		return protocol.E("invalid_params", "Core owner is required")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT agent_id,operation_id FROM plugin_calls WHERE state IN ('dispatched','cancelling') AND owner<>?`, owner)
		if err != nil {
			return err
		}
		var targets []PluginCallTarget
		for rows.Next() {
			var p PluginCallTarget
			if err := rows.Scan(&p.AgentID, &p.OperationID); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range targets {
			r, err := pluginRecord(ctx, tx, p)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plugin_calls SET state='unknown' WHERE operation_id=?`, p.OperationID); err != nil {
				return err
			}
			r.Receipt.State, r.Receipt.Error = "unknown", pluginStateError("unknown")
			if _, err := appendEvent(ctx, tx, p.AgentID, r.Receipt.TaskID, "plugin.unknown", r.Receipt); err != nil {
				return err
			}
		}
		return nil
	})
}

type PluginResultPage struct {
	PluginCallTarget
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
	Offset int    `json:"offset"`
	Next   int    `json:"next"`
	Total  int    `json:"total"`
	More   bool   `json:"more"`
}

func (s *Store) ReadPluginResult(ctx context.Context, target PluginCallTarget, offset, limit int) (PluginResultPage, error) {
	out := PluginResultPage{PluginCallTarget: target, Offset: offset}
	if !pluginID(target.AgentID, true) || !pluginID(target.OperationID, true) || offset < 0 || limit < 0 || limit != 0 && (limit < 4 || limit > 256<<10) {
		return out, protocol.E("invalid_params", "plugin result accepts a nonnegative byte offset and page size 4–262144")
	}
	if limit == 0 {
		limit = 64 << 10
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := pluginRecord(ctx, tx, target)
		if err != nil {
			return err
		}
		if r == nil {
			return protocol.E("not_found", "plugin call not found for this Agent")
		}
		if r.Receipt.ResultBytes == 0 {
			return protocol.E("unavailable", "plugin call has no retained result; inspect its current state")
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT result FROM plugin_calls WHERE operation_id=?`, target.OperationID).Scan(&raw); err != nil {
			return err
		}
		if len(raw) != r.Receipt.ResultBytes || len(raw) > maxPluginResultBytes || pluginDigest([]byte(raw)) != r.Receipt.ResultSHA256 || !utf8.ValidString(raw) {
			return protocol.E("invalid_state", "retained plugin result failed integrity verification")
		}
		if offset > len(raw) || offset < len(raw) && !utf8.RuneStart(raw[offset]) {
			return protocol.E("invalid_params", "result offset must be within retained bytes at a UTF-8 boundary")
		}
		end := min(offset+limit, len(raw))
		for end < len(raw) && !utf8.RuneStart(raw[end]) {
			end--
		}
		out.Text, out.Next, out.Total, out.SHA256 = raw[offset:end], end, len(raw), r.Receipt.ResultSHA256
		out.More = end < len(raw)
		return nil
	})
	return out, err
}
