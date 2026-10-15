package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type Work struct {
	Agent protocol.Agent
	Task  protocol.Task
}
type ContextMessage struct {
	RequestID  string                 `json:"request_id,omitempty"`
	QuestionID string                 `json:"question_id,omitempty"`
	Message    model.Message          `json:"message"`
	Resources  []protocol.ResourceRef `json:"resources,omitempty"`
}

func (s *Store) ReadyAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.id FROM agents a JOIN tasks t ON t.agent_id=a.id WHERE a.state='idle' AND a.paused=0 AND t.state='accepted' ORDER BY a.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimNext is the durable single-loop gate, including competing schedulers.
// Queued inputs become model context only as their task starts.
func (s *Store) ClaimNext(ctx context.Context, id string) (*Work, error) {
	var work *Work
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		w := &Work{}
		a, t := &w.Agent, &w.Task
		err := tx.QueryRowContext(ctx, `SELECT id,coalesce(parent_id,''),cwd,state,model,seq FROM agents WHERE id=? AND state='idle' AND paused=0`, id).Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		*t, err = scanTask(tx.QueryRowContext(ctx, `SELECT id,agent_id,state,input,result,attachments,skills FROM tasks WHERE agent_id=? AND state='accepted' ORDER BY priority DESC,rowid LIMIT 1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET state='running' WHERE id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='running' WHERE id=?", t.ID); err != nil {
			return err
		}
		a.State, t.State = "running", "running"
		if _, err := appendEvent(ctx, tx, id, t.ID, "task.started", *t); err != nil {
			return err
		}
		var content []model.Block
		if t.Input != "" {
			content = append(content, model.Block{Type: "text", Text: t.Input})
		}
		for _, ref := range t.Attachments {
			content = append(content, model.Block{Type: "image", Attachment: ref})
		}
		skills, err := selectedSkills(ctx, tx, id, t.Skills)
		if err != nil {
			return err
		}
		content = append(content, skills...)
		if _, err := appendEvent(ctx, tx, id, t.ID, "context.message", ContextMessage{Resources: t.Skills, Message: model.Message{Role: "user", Content: content}}); err != nil {
			return err
		}
		work = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return work, nil
}

func runningTask(ctx context.Context, tx *sql.Tx, agent, task string) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=? AND agent_id=?`, task, agent).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || err == nil && state != "running" && state != "cancelling" {
		return protocol.E("conflict", "task is no longer running")
	}
	return err
}

func (s *Store) AppendTaskEvent(ctx context.Context, agent, task, kind string, payload any) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := runningTask(ctx, tx, agent, task); err != nil {
			return err
		}
		if kind == "model.started" || kind == "tool.started" || kind == "tool.dispatching" {
			if err := activeTask(ctx, tx, agent, task); err != nil {
				return err
			}
		}
		_, err := appendEvent(ctx, tx, agent, task, kind, payload)
		return err
	})
}

// The response event and every complete message are committed together. Raw
// deltas are independent append-only records, including interrupted responses.
func (s *Store) RecordResponse(ctx context.Context, agent, task, request string, response model.Response) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := runningTask(ctx, tx, agent, task); err != nil {
			return err
		}
		for _, message := range response.Messages {
			if _, err := appendEvent(ctx, tx, agent, task, "context.message", ContextMessage{RequestID: request, Message: message}); err != nil {
				return err
			}
		}
		_, err := appendEvent(ctx, tx, agent, task, "model.completed", struct {
			RequestID string                `json:"request_id"`
			Usage     *model.Usage          `json:"usage,omitempty"`
			Output    []model.PublicMessage `json:"output,omitempty"`
		}{request, response.Usage, response.Output})
		return err
	})
}

// FormatRepair and its developer message commit together. The original
// assistant messages and tool results stay in the append-only context.
func (s *Store) RecordFormatRepair(ctx context.Context, agent, task, request string, attempt, limit int, instruction string) error {
	if attempt < 1 || attempt > limit || instruction == "" {
		return protocol.E("invalid_params", "format repair requires an instruction and an attempt within its limit")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := activeTask(ctx, tx, agent, task); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, agent, task, "model.repair", struct {
			RequestID string `json:"request_id"`
			Attempt   int    `json:"attempt"`
			Limit     int    `json:"limit"`
		}{request, attempt, limit}); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, agent, task, "context.message", ContextMessage{RequestID: request, Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: instruction}}}})
		return err
	})
}

func (s *Store) RecordToolResult(ctx context.Context, agent, task, request string, result any, message model.Message) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := runningTask(ctx, tx, agent, task); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, agent, task, "tool.completed", result); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, agent, task, "context.message", ContextMessage{RequestID: request, Message: message})
		return err
	})
}

func (s *Store) ModelContext(ctx context.Context, agent string) ([]model.Message, int64, error) {
	messages := []model.Message{}
	var cursor int64
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", agent).Scan(&cursor); err != nil {
			return err
		}
		entries, err := contextEntries(ctx, tx, agent, cursor)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			messages = append(messages, entry.Record.Message)
		}
		return nil
	})
	return messages, cursor, err
}

type contextEntry struct {
	Seq    int64
	Record ContextMessage
}

// Shared by requests and inheritance. Active compaction views belong here;
// callers must not reconstruct a second context by reading the raw journal.
func contextEntries(ctx context.Context, tx *sql.Tx, agent string, cursor int64) ([]contextEntry, error) {
	rows, err := tx.QueryContext(ctx, "SELECT seq,payload FROM events WHERE agent_id=? AND kind='context.message' AND seq<=? ORDER BY seq", agent, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []contextEntry
	for rows.Next() {
		var entry contextEntry
		var data []byte
		if err := rows.Scan(&entry.Seq, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &entry.Record); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) FinishTask(ctx context.Context, agent, task, state, result string, failure *protocol.Error) error {
	if state != "completed" && state != "failed" && state != "unknown" && state != "cancelled" {
		return protocol.E("invalid_params", "invalid loop completion state")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := runningTask(ctx, tx, agent, task); err != nil {
			return err
		}
		var previous string
		var agentBefore string
		var paused bool
		if err := tx.QueryRowContext(ctx, "SELECT t.state,a.paused,a.state FROM tasks t JOIN agents a ON a.id=t.agent_id WHERE t.id=?", task).Scan(&previous, &paused, &agentBefore); err != nil {
			return err
		}
		if agentBefore == "unknown" && previous == "cancelling" && state != "unknown" {
			return protocol.E("operation_unknown", "cancellation requires verification of the unresolved effect")
		}
		if previous == "cancelling" {
			if state == "unknown" {
				// The worker returned, but its side effect is still unresolved.
				// Keep cancellation pending and hold even an accepted resume.
				if _, err := tx.ExecContext(ctx, "UPDATE agents SET state='unknown' WHERE id=?", agent); err != nil {
					return err
				}
				_, err := appendEvent(ctx, tx, agent, task, "task.cancellation_pending", map[string]any{"error": failure})
				return err
			}
			state, result = "cancelled", ""
		} else if state == "cancelled" {
			return protocol.E("conflict", "task has no cancellation request")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state=?,result=? WHERE id=?", state, result, task); err != nil {
			return err
		}
		agentState := "idle"
		if paused {
			agentState = "paused"
		}
		if state == "unknown" {
			agentState = "unknown"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET state=? WHERE id=?", agentState, agent); err != nil {
			return err
		}
		if state == "cancelled" {
			if err := cancellationContext(ctx, tx, agent, task); err != nil {
				return err
			}
		}
		if state != "unknown" {
			if err := finishQuestions(ctx, tx, agent, task); err != nil {
				return err
			}
		}
		_, err := appendEvent(ctx, tx, agent, task, "task."+state, struct {
			State  string          `json:"state"`
			Result string          `json:"result,omitempty"`
			Error  *protocol.Error `json:"error,omitempty"`
		}{state, result, failure})
		return err
	})
}

// Until recovery has evidence for a safe continuation, an old running task is
// held rather than replayed. Called once by the elected core before scheduling.
func (s *Store) HoldInterrupted(ctx context.Context) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT t.id,t.agent_id,t.state FROM tasks t JOIN agents a ON a.id=t.agent_id WHERE t.state='running' OR (t.state='cancelling' AND a.state!='unknown')")
		if err != nil {
			return err
		}
		type pair struct{ task, agent, state string }
		var pending []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.task, &p.agent, &p.state); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range pending {
			if p.state == "running" {
				if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='unknown' WHERE id=?", p.task); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE agents SET state='unknown' WHERE id=?", p.agent); err != nil {
				return err
			}
			kind := "task.unknown"
			if p.state == "cancelling" {
				kind = "task.cancellation_pending"
			}
			if _, err := appendEvent(ctx, tx, p.agent, p.task, kind, map[string]string{"reason": "core interrupted the model loop; inspect persisted requests and calls before continuing"}); err != nil {
				return err
			}
		}
		return nil
	})
}
