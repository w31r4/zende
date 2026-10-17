package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

type PendingTreeStop struct {
	ID     string
	Cursor int64 // Internal SQLite scan cursor, not a public event sequence.
}

func (s *Store) PendingTreeStops(ctx context.Context, after int64) ([]PendingTreeStop, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT json_extract(e.payload,'$.operation_id'),e.rowid FROM events e
 WHERE e.kind='tree_stop.started' AND e.source_agent='' AND e.rowid>? AND NOT EXISTS(
 SELECT 1 FROM events d WHERE d.agent_id=e.agent_id AND d.kind='tree_stop.completed'
 AND json_extract(d.payload,'$.operation_id')=json_extract(e.payload,'$.operation_id')) ORDER BY e.rowid LIMIT 32`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PendingTreeStop
	for rows.Next() {
		var p PendingTreeStop
		if err := rows.Scan(&p.ID, &p.Cursor); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) ActiveTreeStop(ctx context.Context, agent string) (string, error) {
	err := treeWritable(ctx, s.db, agent)
	var wire *protocol.Error
	if errors.As(err, &wire) && wire.Data.Kind == "tree_stopping" {
		return wire.Data.OperationID, nil
	}
	return "", err
}

type treeProgress struct {
	OperationID string          `json:"operation_id"`
	Stage       string          `json:"stage"`
	Failure     *protocol.Error `json:"failure,omitempty"`
}

func treeStopProgress(ctx context.Context, q rowQuerier, agent, id string) (treeProgress, error) {
	var result treeProgress
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='tree_stop.progress'
 AND json_extract(payload,'$.operation_id')=? ORDER BY seq DESC LIMIT 1`, agent, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	err = protocol.StrictDecode(data, &result)
	return result, err
}

func (s *Store) RecordTreeStopProgress(ctx context.Context, id, stage string, failure *protocol.Error) error {
	switch stage {
	case "processes", "worktrees", "tasks", "release":
	default:
		return protocol.E("invalid_params", "invalid tree stop stage")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, err := treeStop(ctx, tx, id)
		if err != nil {
			return err
		}
		if plan.State == "stopped" {
			return nil
		}
		if plan.Stage == stage && reflect.DeepEqual(plan.Failure, failure) {
			return nil
		}
		_, err = appendEvent(ctx, tx, plan.AgentID, "", "tree_stop.progress", treeProgress{id, stage, failure})
		return err
	})
}

type treeRunnerScope struct {
	OperationID string            `json:"operation_id"`
	Scope       process.StopScope `json:"scope"`
}

func decodeTreeRunnerScope(data []byte, plan TreeStopRecord) (process.StopScope, error) {
	var saved treeRunnerScope
	if err := protocol.StrictDecode(data, &saved); err != nil {
		return process.StopScope{}, err
	}
	agents := make([]string, 0, len(plan.Members))
	for _, member := range plan.Members {
		agents = append(agents, member.AgentID)
	}
	slices.Sort(agents)
	got := slices.Clone(saved.Scope.Agents)
	slices.Sort(got)
	if saved.OperationID != plan.OperationID || !slices.Equal(agents, got) {
		return process.StopScope{}, protocol.E("invalid_state", "runner scope differs from the original tree stop plan")
	}
	return saved.Scope, nil
}

// The Agent fence already prevents new dispatches. Freeze all original exec
// identities, including historical/legacy calls whose delayed RPC may arrive
// after their client vanished. The saved scope never expands on retry.
func (s *Store) TreeStopRunnerScope(ctx context.Context, id string) (process.StopScope, error) {
	var result process.StopScope
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, err := treeStop(ctx, tx, id)
		if err != nil {
			return err
		}
		var data []byte
		err = tx.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='tree_stop.runner_scope'
 AND json_extract(payload,'$.operation_id')=? ORDER BY seq LIMIT 1`, plan.AgentID, id).Scan(&data)
		if err == nil {
			result, err = decodeTreeRunnerScope(data, plan)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if plan.State == "stopped" {
			return protocol.E("invalid_state", "completed tree stop has no runner scope")
		}
		result = process.StopScope{Agents: []string{}, Pending: []process.StopDispatch{}}
		seen := map[string]process.StopDispatch{}
		for _, member := range plan.Members {
			result.Agents = append(result.Agents, member.AgentID)
			rows, err := tx.QueryContext(ctx, `SELECT DISTINCT task_id,json_extract(payload,'$.operation_id') FROM events
 WHERE agent_id=? AND source_agent='' AND json_extract(payload,'$.call.name')='exec_command' AND
 (kind='tool.dispatching' OR (kind='tool.started' AND coalesce(json_extract(payload,'$.schema_version'),1)<2)) LIMIT 10001`, member.AgentID)
			if err != nil {
				return err
			}
			for rows.Next() {
				p := process.StopDispatch{AgentID: member.AgentID}
				if err := rows.Scan(&p.TaskID, &p.ID); err != nil {
					rows.Close()
					return err
				}
				if old, ok := seen[p.ID]; ok && old != p {
					rows.Close()
					return protocol.E("invalid_state", "process dispatch identity has conflicting owners")
				}
				seen[p.ID] = p
				if len(seen) > 10000 {
					rows.Close()
					return protocol.E("limit_exceeded", "tree stop exceeds 10000 dispatched process identities")
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		for _, p := range seen {
			result.Pending = append(result.Pending, p)
		}
		slices.Sort(result.Agents)
		slices.SortFunc(result.Pending, func(a, b process.StopDispatch) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		_, err = appendEvent(ctx, tx, plan.AgentID, "", "tree_stop.runner_scope", treeRunnerScope{id, result})
		return err
	})
	return result, err
}

// Readiness is a transaction over core-owned state. Runner release follows it;
// completion checks it again before releasing the Agent admission fence.
func treeStopReady(ctx context.Context, q rowQuerier, plan TreeStopRecord) error {
	for _, member := range plan.Members {
		var paused bool
		if err := q.QueryRowContext(ctx, "SELECT paused FROM agents WHERE id=?", member.AgentID).Scan(&paused); err != nil {
			return err
		}
		if !paused {
			return protocol.E("invalid_state", "tree stop member is not paused")
		}
		var pendingPlugin bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM plugin_calls WHERE agent_id=? AND state IN ('dispatched','cancelling','unknown')) OR EXISTS(SELECT 1 FROM plugin_subscription_heads WHERE agent_id=? AND json_extract(payload,'$.state') IN ('opening','open','closing','unknown'))`, member.AgentID, member.AgentID).Scan(&pendingPlugin); err != nil {
			return err
		}
		if pendingPlugin {
			return protocol.E("tree_stopping", "plugin calls are still settling or unverified; inspect their original operations")
		}
		if member.TaskID == "" {
			continue
		}
		var state string
		if err := q.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id=? AND agent_id=?", member.TaskID, member.AgentID).Scan(&state); err != nil {
			return err
		}
		if state != "cancelled" && state != "completed" && state != "failed" {
			return protocol.E("tree_stopping", "captured tasks are still settling; inspect their recovery records")
		}
	}
	for _, id := range plan.Worktrees {
		var state string
		if err := q.QueryRowContext(ctx, "SELECT state FROM workspace_operations WHERE id=?", id).Scan(&state); err != nil {
			return err
		}
		if state != "cancelled" {
			return protocol.E("tree_stopping", "captured workspace creation is still settling")
		}
	}
	return nil
}

func (s *Store) TreeStopReady(ctx context.Context, id string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, err := treeStop(ctx, tx, id)
		if err != nil {
			return err
		}
		return treeStopReady(ctx, tx, plan)
	})
}

func (s *Store) CompleteTreeStop(ctx context.Context, id string, receipt process.StopRecord) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, err := treeStop(ctx, tx, id)
		if err != nil {
			return err
		}
		if plan.State == "stopped" {
			return nil
		}
		var data []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='tree_stop.runner_scope'
 AND json_extract(payload,'$.operation_id')=? ORDER BY seq LIMIT 1`, plan.AgentID, id).Scan(&data); err != nil {
			return err
		}
		scope, err := decodeTreeRunnerScope(data, plan)
		if err != nil {
			return err
		}
		if err := receipt.Matches(id, scope); err != nil {
			return err
		}
		if receipt.State != "released" {
			return protocol.E("tree_stopping", "runner scope has not been released")
		}
		if err := treeStopReady(ctx, tx, plan); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, plan.AgentID, "", "tree_stop.completed", map[string]string{"operation_id": id, "runner_operation_id": receipt.OperationID})
		return err
	})
}
