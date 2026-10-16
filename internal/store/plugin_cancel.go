package store

import (
	"context"
	"database/sql"

	"zende.local/harness/internal/protocol"
)

func markPluginCancelling(ctx context.Context, tx *sql.Tx, r *PluginRecord) error {
	if r.Receipt.State != "dispatched" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_calls SET state='cancelling' WHERE operation_id=?`, r.Receipt.OperationID); err != nil {
		return err
	}
	r.Receipt.State = "cancelling"
	_, err := appendEvent(ctx, tx, r.Receipt.AgentID, r.Receipt.TaskID, "plugin.cancelling", r.Receipt)
	return err
}

// CancelPluginCall saves user intent and its retry receipt before the host may
// signal the original worker. A verified terminal result always stays intact.
func (s *Store) CancelPluginCall(ctx context.Context, req protocol.Request) (protocol.Accepted, error) {
	var target protocol.PluginCallTarget
	if req.Validate() != nil || req.Method != "plugin.cancel" || !pluginID(req.OperationID, true) || protocol.StrictDecode(req.Params, &target) != nil || !pluginID(target.AgentID, true) || !pluginID(target.OperationID, true) {
		return protocol.Accepted{}, protocol.E("invalid_params", "plugin cancellation requires Agent, original operation and a new request ID")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		r, err := pluginRecord(ctx, tx, target)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if r == nil {
			return protocol.Accepted{}, protocol.E("not_found", "plugin call not found for this Agent")
		}
		if err := markPluginCancelling(ctx, tx, r); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: target.AgentID, TaskID: r.Receipt.TaskID}, nil
	})
}

// A Task stop commits plugin cancellation in the same transaction as its own
// state change. Runtime notification happens only after that commit succeeds.
func cancelTaskPluginCalls(ctx context.Context, tx *sql.Tx, agent, task string) error {
	rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM plugin_calls WHERE agent_id=? AND task_id=? AND state='dispatched'`, agent, task)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		r, err := pluginRecord(ctx, tx, PluginCallTarget{AgentID: agent, OperationID: id})
		if err != nil {
			return err
		}
		if r == nil {
			return protocol.E("invalid_state", "pending plugin call disappeared during Task cancellation")
		}
		if err := markPluginCancelling(ctx, tx, r); err != nil {
			return err
		}
	}
	return nil
}
