package store

import (
	"context"
	"database/sql"
	"errors"

	"zende.local/harness/internal/protocol"
)

// TaskJournal reads one consistent boundary and streams lifecycle records to a
// pure consumer. Raw model deltas remain in history but are not needed to prove
// dispatch or completion. The callback must not call Store or perform I/O.
func (s *Store) TaskJournal(ctx context.Context, agent, task string, consume func(protocol.Event) error) (Work, error) {
	var w Work
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		a, t := &w.Agent, &w.Task
		if err := tx.QueryRowContext(ctx, "SELECT id,coalesce(parent_id,''),cwd,state,model,seq,paused FROM agents WHERE id=?", agent).Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq, &a.Paused); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("not_found", "agent not found")
			}
			return err
		}
		query := "SELECT id,agent_id,state,input,result,attachments FROM tasks WHERE agent_id=?"
		args := []any{agent}
		if task != "" {
			query += " AND id=?"
			args = append(args, task)
		} else {
			query += " ORDER BY CASE WHEN state IN ('running','cancelling','unknown') THEN 0 ELSE 1 END,rowid DESC LIMIT 1"
		}
		var err error
		*t, err = scanTask(tx.QueryRowContext(ctx, query, args...))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("not_found", "task not found")
			}
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT seq,kind,payload FROM events WHERE agent_id=? AND task_id=? AND seq<=? AND kind NOT IN ('model.delta','model.output.delta') ORDER BY seq`, agent, t.ID, a.Seq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := protocol.Event{Version: protocol.Version, AgentID: agent, TaskID: t.ID}
			var payload string
			if err := rows.Scan(&e.Seq, &e.Kind, &payload); err != nil {
				return err
			}
			e.Payload = []byte(payload)
			if consume != nil {
				if err := consume(e); err != nil {
					return err
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return Work{}, err
	}
	return w, nil
}
