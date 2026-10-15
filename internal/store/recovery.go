package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type HeldTask struct {
	AgentID    string
	TaskID     string
	RequestSeq int64
}

// One automatic attempt per core lifetime; explicit recover requests advance
// RequestSeq. Ordinary input must not authorize retries or release this gate.
func (s *Store) HeldTasks(ctx context.Context) ([]HeldTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,t.id,coalesce((SELECT max(seq) FROM events e WHERE e.agent_id=a.id AND e.task_id=t.id AND e.kind='recovery.requested'),0) FROM agents a JOIN tasks t ON t.agent_id=a.id WHERE a.state='unknown' AND t.state IN ('unknown','cancelling') ORDER BY t.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []HeldTask
	for rows.Next() {
		var task HeldTask
		if err := rows.Scan(&task.AgentID, &task.TaskID, &task.RequestSeq); err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func (s *Store) RequestRecovery(ctx context.Context, req protocol.Request, target protocol.RecoveryTarget) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		query := `SELECT t.id FROM tasks t JOIN agents a ON a.id=t.agent_id WHERE a.id=? AND a.state='unknown' AND t.state IN ('unknown','cancelling')`
		args := []any{target.AgentID}
		if target.TaskID != "" {
			query += " AND t.id=?"
			args = append(args, target.TaskID)
		}
		query += " ORDER BY t.rowid LIMIT 1"
		var task string
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&task); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.Accepted{}, protocol.E("conflict", "no held task matches this recovery request")
			}
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, target.AgentID, task, "recovery.requested", map[string]string{"operation_id": req.OperationID}); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: target.AgentID, TaskID: task}, nil
	})
}

func heldTask(ctx context.Context, tx *sql.Tx, agent, task string, cursor int64) (Work, error) {
	var w Work
	a, t := &w.Agent, &w.Task
	var refs string
	err := tx.QueryRowContext(ctx, `SELECT a.id,coalesce(a.parent_id,''),a.cwd,a.state,a.model,a.seq,a.paused,t.id,t.agent_id,t.state,t.input,t.result,t.attachments FROM agents a JOIN tasks t ON t.agent_id=a.id WHERE a.id=? AND t.id=?`, agent, task).Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq, &a.Paused, &t.ID, &t.AgentID, &t.State, &t.Input, &t.Result, &refs)
	if err != nil {
		return Work{}, err
	}
	if err := json.Unmarshal([]byte(refs), &t.Attachments); err != nil {
		return Work{}, err
	}
	if a.Seq != cursor || a.State != "unknown" || t.State != "unknown" && t.State != "cancelling" {
		return Work{}, protocol.E("recovery_conflict", "task changed while recovery was verifying its effects; inspect the current records")
	}
	return w, nil
}

type VerifiedResult struct {
	RequestID   string
	OperationID string
	Outcome     any
	Message     model.Message
}

// Only the runtime supplies verified results. This is not an external API for
// acknowledging arbitrary effects. The held gate remains until ActivateHeld.
func (s *Store) RecordVerifiedResults(ctx context.Context, agent, task string, cursor int64, results []VerifiedResult) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := heldTask(ctx, tx, agent, task, cursor); err != nil {
			return err
		}
		for _, r := range results {
			if _, err := appendEvent(ctx, tx, agent, task, "tool.verified", map[string]string{"request_id": r.RequestID, "operation_id": r.OperationID}); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, agent, task, "tool.completed", r.Outcome); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, agent, task, "context.message", ContextMessage{RequestID: r.RequestID, Message: r.Message}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RecordRecoveryFailure(ctx context.Context, agent, task string, cursor int64, failure *protocol.Error) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := heldTask(ctx, tx, agent, task, cursor); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, agent, task, "recovery.held", map[string]any{"error": failure})
		return err
	})
}

// Runtime calls this only after checking the complete journal and all unknown
// effects. The seq comparison fences concurrent stop/input/config decisions.
func (s *Store) ActivateHeld(ctx context.Context, agent, task string, cursor int64) (Work, error) {
	var w Work
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		w, err = heldTask(ctx, tx, agent, task, cursor)
		if err != nil {
			return err
		}
		state := "running"
		if w.Agent.Paused || w.Task.State == "cancelling" {
			state = "cancelling"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state=? WHERE id=?", state, task); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET state=? WHERE id=?", state, agent); err != nil {
			return err
		}
		w.Agent.State, w.Task.State = state, state
		seq, err := appendEvent(ctx, tx, agent, task, "task.recovered", map[string]any{"from_cursor": cursor, "state": state})
		w.Agent.Seq = seq.Seq
		return err
	})
	return w, err
}
