package store

import (
	"context"
	"database/sql"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// Stop commits the pause and cancellation request before the runtime signals
// its in-memory worker. Retrying this operation never targets a newer Task.
func (s *Store) Stop(ctx context.Context, req protocol.Request, p protocol.Target) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM agents WHERE id=?", p.AgentID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.Accepted{}, protocol.E("not_found", "agent not found")
			}
			return protocol.Accepted{}, err
		}
		if state == "archived" || state == "copying" {
			return protocol.Accepted{}, protocol.E("conflict", "agent is read-only")
		}
		var task string
		err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE agent_id=? AND state IN ('running','cancelling','unknown') ORDER BY rowid LIMIT 1`, p.AgentID).Scan(&task)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, err
		}
		next := "paused"
		if task != "" {
			next = "cancelling"
		}
		if state == "unknown" {
			next = "unknown"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET paused=1,state=? WHERE id=?", next, p.AgentID); err != nil {
			return protocol.Accepted{}, err
		}
		if task != "" {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='cancelling' WHERE id=?", task); err != nil {
				return protocol.Accepted{}, err
			}
			if _, err := appendEvent(ctx, tx, p.AgentID, task, "task.cancelling", map[string]string{"operation_id": req.OperationID}); err != nil {
				return protocol.Accepted{}, err
			}
		}
		if _, err := appendEvent(ctx, tx, p.AgentID, task, "agent.paused", map[string]any{"operation_id": req.OperationID, "paused": true, "state": next}); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: p.AgentID, TaskID: task}, nil
	})
}

func unpause(ctx context.Context, tx *sql.Tx, agent, task string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET paused=0,state=CASE WHEN state='paused' THEN 'idle' ELSE state END WHERE id=?`, agent); err != nil {
		return err
	}
	_, err := appendEvent(ctx, tx, agent, task, "agent.resumed", map[string]any{"paused": false, "next_task_id": task})
	return err
}

func (s *Store) Resume(ctx context.Context, req protocol.Request, p protocol.Target, maxAgents int) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		var state string
		var paused bool
		if err := tx.QueryRowContext(ctx, "SELECT state,paused FROM agents WHERE id=?", p.AgentID).Scan(&state, &paused); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.Accepted{}, protocol.E("not_found", "agent not found")
			}
			return protocol.Accepted{}, err
		}
		if state == "unknown" {
			return protocol.Accepted{}, protocol.E("operation_unknown", "inspect unresolved operations before resuming")
		}
		if !paused || state == "copying" || state == "archived" {
			return protocol.Accepted{}, protocol.E("conflict", "agent is not resumable from this state")
		}
		if err := checkCapacity(ctx, tx, p.AgentID, maxAgents); err != nil {
			return protocol.Accepted{}, err
		}
		queued, err := nextQueuedTask(ctx, tx, p.AgentID)
		task := queued.ID
		if errors.Is(err, sql.ErrNoRows) {
			next, e := createTask(ctx, tx, p.AgentID, "Continue from the saved context. Inspect prior tool results and running processes; do not repeat completed actions.")
			err = e
			task = next.ID
		}
		if err != nil {
			return protocol.Accepted{}, err
		}
		if err := unpause(ctx, tx, p.AgentID, task); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: p.AgentID, TaskID: task}, nil
	})
}

func (s *Store) CancellationRequested(ctx context.Context, agent, task string) (bool, error) {
	var state string
	err := s.db.QueryRowContext(ctx, "SELECT state FROM tasks WHERE agent_id=? AND id=?", agent, task).Scan(&state)
	return state == "cancelling", err
}

// ActiveTask fences new model requests and tool intentions against a committed
// stop. Observations and already-running tool results use runningTask instead.
func activeTask(ctx context.Context, tx *sql.Tx, agent, task string) error {
	if err := runningTask(ctx, tx, agent, task); err != nil {
		return err
	}
	var state string
	var paused bool
	if err := tx.QueryRowContext(ctx, "SELECT t.state,a.paused FROM tasks t JOIN agents a ON a.id=t.agent_id WHERE t.id=? AND a.id=?", task, agent).Scan(&state, &paused); err != nil {
		return err
	}
	if state == "cancelling" || paused {
		return protocol.E("task_cancelled", "task cancellation was requested")
	}
	return nil
}

func cancellationContext(ctx context.Context, tx *sql.Tx, agent, task string) error {
	_, err := appendEvent(ctx, tx, agent, task, "context.message", ContextMessage{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "The preceding task was cancelled by the user. Completed tool effects and results remain valid; background processes may still be running. Resume is a new task. Inspect existing results and process identities before taking further action."}}}})
	return err
}
