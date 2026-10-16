// Package store persists append-only facts and transactional state projections.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
	"zende.local/harness/internal/protocol"
)

const SchemaVersion = 16

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > SchemaVersion {
		return fmt.Errorf("unsupported database schema %d", version)
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	if version == SchemaVersion {
		return nil
	}
	return s.Transaction(context.Background(), func(tx *sql.Tx) error {
		if version == 0 {
			if _, err := tx.Exec(schema); err != nil {
				return err
			}
		}
		if version < 2 {
			if _, err := tx.Exec(`ALTER TABLE agents ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=2;`); err != nil {
				return err
			}
		}
		if version < 3 {
			if _, err := tx.Exec(attachmentSchema); err != nil {
				return err
			}
		}
		if version < 4 {
			if _, err := tx.Exec(questionSchema); err != nil {
				return err
			}
		}
		if version < 5 {
			if _, err := tx.Exec(resourceSchema); err != nil {
				return err
			}
		}
		if version < 6 {
			if _, err := tx.Exec(skillInputSchema); err != nil {
				return err
			}
		}
		if version < 7 {
			if _, err := tx.Exec(selectionSchema); err != nil {
				return err
			}
		}
		// v8 adds context.view events; v9 records external worktree preparation.
		if version < 9 {
			if _, err := tx.Exec(worktreeSchema); err != nil {
				return err
			}
		}
		if version < 10 {
			if _, err := tx.Exec(mailboxSchema); err != nil {
				return err
			}
		}
		if version < 11 {
			if _, err := tx.Exec(taskResultSchema); err != nil {
				return err
			}
		}
		// v12 adds durable tree-stop events. Older binaries must not schedule
		// work without honoring their admission fence.
		if version < 12 {
			if _, err := tx.Exec(treeStopSchema); err != nil {
				return err
			}
		}
		if version < 13 {
			if _, err := tx.Exec(resourceUpdateSchema); err != nil {
				return err
			}
		}
		if version < 14 {
			if _, err := tx.Exec(mcpCallSchema); err != nil {
				return err
			}
		}
		if version < 15 {
			if _, err := tx.Exec(pluginCallSchema); err != nil {
				return err
			}
		}
		_, err := tx.Exec(pluginSubscriptionSchema)
		return err
	})
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE agents (
 id TEXT PRIMARY KEY, parent_id TEXT REFERENCES agents(id), cwd TEXT NOT NULL,
 state TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tasks (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id),
 state TEXT NOT NULL, input TEXT NOT NULL, result TEXT NOT NULL DEFAULT ''
);
CREATE INDEX task_agent ON tasks(agent_id);
CREATE TABLE events (
 agent_id TEXT NOT NULL REFERENCES agents(id), seq INTEGER NOT NULL,
 kind TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL,
 PRIMARY KEY(agent_id,seq)
);
CREATE TABLE operations (
 id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result TEXT NOT NULL
);
CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'append-only history'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'append-only history'); END;
CREATE TRIGGER operations_no_update BEFORE UPDATE ON operations BEGIN SELECT RAISE(ABORT,'immutable acceptance'); END;
CREATE TRIGGER operations_no_delete BEFORE DELETE ON operations BEGIN SELECT RAISE(ABORT,'immutable acceptance'); END;
PRAGMA user_version=1;
`

func appendEvent(ctx context.Context, tx *sql.Tx, agent, task, kind string, payload any) (protocol.Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return protocol.Event{}, err
	}
	var seq int64
	err = tx.QueryRowContext(ctx, "UPDATE agents SET seq=seq+1 WHERE id=? RETURNING seq", agent).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Event{}, protocol.E("not_found", "agent not found")
	}
	if err != nil {
		return protocol.Event{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(agent_id,seq,kind,task_id,payload) VALUES(?,?,?,?,?)", agent, seq, kind, task, string(b))
	return protocol.Event{Version: protocol.Version, AgentID: agent, Seq: seq, Kind: kind, TaskID: task, Payload: b}, err
}

func (s *Store) Append(ctx context.Context, agent, task, kind string, payload any) (protocol.Event, error) {
	var event protocol.Event
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		event, err = appendEvent(ctx, tx, agent, task, kind, payload)
		return err
	})
	if err != nil {
		return protocol.Event{}, err
	}
	return event, nil
}

// accept commits the mutation and its deduplication record together. No accepted
// identity escapes to a caller when the commit fails.
func (s *Store) accept(ctx context.Context, req protocol.Request, fn func(*sql.Tx) (protocol.Accepted, error)) (protocol.Accepted, error) {
	if req.OperationID == "" {
		return protocol.Accepted{}, protocol.E("invalid_params", "operation_id is required for a mutation")
	}
	fingerprint, err := req.EffectFingerprint()
	if err != nil {
		return protocol.Accepted{}, err
	}
	var accepted protocol.Accepted
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var oldHash, result string
		err := tx.QueryRowContext(ctx, "SELECT fingerprint,result FROM operations WHERE id=?", req.OperationID).Scan(&oldHash, &result)
		if err == nil {
			if oldHash != fingerprint {
				return protocol.E("operation_conflict", "operation_id was already used for a different request")
			}
			return json.Unmarshal([]byte(result), &accepted)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := worktreeFingerprint(ctx, tx, req.OperationID, fingerprint); err != nil {
			return err
		}
		accepted, err = fn(tx)
		if err != nil {
			return err
		}
		accepted.OperationID = req.OperationID
		accepted.State = "accepted"
		b, err := json.Marshal(accepted)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO operations(id,fingerprint,result) VALUES(?,?,?)", req.OperationID, fingerprint, string(b))
		return err
	})
	if err != nil {
		return protocol.Accepted{}, err
	}
	return accepted, nil
}

func (s *Store) Create(ctx context.Context, req protocol.Request, p protocol.CreateAgent, maxAgents int) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if p.Input != "" {
			if err := checkCapacity(ctx, tx, "", maxAgents); err != nil {
				return protocol.Accepted{}, err
			}
		}
		a := protocol.Agent{ID: protocol.NewID("agent"), Cwd: p.Cwd, State: "idle", Model: p.Model}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agents(id,cwd,state,model) VALUES(?,?,?,?)", a.ID, a.Cwd, a.State, a.Model); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, a.ID, "", "agent.created", a); err != nil {
			return protocol.Accepted{}, err
		}
		accepted := protocol.Accepted{AgentID: a.ID}
		if p.Input != "" {
			task, err := createTask(ctx, tx, a.ID, p.Input)
			if err != nil {
				return protocol.Accepted{}, err
			}
			accepted.TaskID = task.ID
		}
		return accepted, nil
	})
}

func createTask(ctx context.Context, tx *sql.Tx, agent, input string, attachments ...string) (protocol.Task, error) {
	return createTaskWithSkills(ctx, tx, agent, input, nil, attachments)
}

func createTaskWithSkills(ctx context.Context, tx *sql.Tx, agent, input string, skills []protocol.ResourceRef, attachments []string) (protocol.Task, error) {
	if err := validateAttachments(ctx, tx, agent, attachments); err != nil {
		return protocol.Task{}, err
	}
	if _, err := selectedSkills(ctx, tx, agent, skills); err != nil {
		return protocol.Task{}, err
	}
	t := protocol.Task{ID: protocol.NewID("task"), AgentID: agent, State: "accepted", Input: input, Attachments: append([]string(nil), attachments...), Skills: append([]protocol.ResourceRef(nil), skills...)}
	refs, err := json.Marshal(t.Attachments)
	if err != nil {
		return protocol.Task{}, err
	}
	skillRefs, err := json.Marshal(t.Skills)
	if err != nil {
		return protocol.Task{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO tasks(id,agent_id,state,input,attachments,skills) VALUES(?,?,?,?,?,?)", t.ID, agent, t.State, input, string(refs), string(skillRefs))
	if err == nil {
		_, err = appendEvent(ctx, tx, agent, t.ID, "task.accepted", t)
	}
	return t, err
}

func (s *Store) Submit(ctx context.Context, req protocol.Request, p protocol.Submit, maxAgents int) (protocol.Accepted, error) {
	if p.Input == "" && len(p.Attachments) == 0 && len(p.Skills) == 0 {
		return protocol.Accepted{}, protocol.E("invalid_params", "input, images or skills are required")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := treeWritable(ctx, tx, p.AgentID); err != nil {
			return protocol.Accepted{}, err
		}
		if err := treeCallerWritable(ctx, tx, p.Caller); err != nil {
			return protocol.Accepted{}, err
		}
		var state string
		var paused bool
		err := tx.QueryRowContext(ctx, "SELECT state,paused FROM agents WHERE id=?", p.AgentID).Scan(&state, &paused)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, protocol.E("not_found", "agent not found")
		}
		if err != nil {
			return protocol.Accepted{}, err
		}
		if state == "archived" || state == "copying" {
			return protocol.Accepted{}, protocol.E("conflict", "agent is read-only")
		}
		origin, err := callSource(ctx, tx, p.Caller)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if err := checkCapacity(ctx, tx, p.AgentID, maxAgents); err != nil {
			return protocol.Accepted{}, err
		}
		task, err := createTaskWithSkills(ctx, tx, p.AgentID, p.Input, p.Skills, p.Attachments)
		if err == nil && origin != nil {
			_, err = appendEvent(ctx, tx, p.AgentID, task.ID, "task.submitted", struct {
				OperationID string               `json:"operation_id"`
				Source      *protocol.CallSource `json:"source"`
			}{req.OperationID, origin})
		}
		if err == nil && paused && state != "unknown" {
			// Explicit new input takes precedence over the existing queue when
			// waking a manually paused Agent. It never revives the old Task.
			_, err = tx.ExecContext(ctx, `UPDATE tasks SET priority=(SELECT coalesce(max(priority),0)+1 FROM tasks WHERE agent_id=?) WHERE id=?`, p.AgentID, task.ID)
			if err == nil {
				err = unpause(ctx, tx, p.AgentID, task.ID)
			}
		}
		return protocol.Accepted{AgentID: p.AgentID, TaskID: task.ID}, err
	})
}

// Limits count active sessions, not stored history or the length of one queue.
func checkCapacity(ctx context.Context, tx *sql.Tx, agent string, maxAgents int) error {
	var count, own int
	const active = "state IN ('accepted','running','cancelling','unknown')"
	const reserved = "SELECT agent_id FROM tasks WHERE " + active + " UNION SELECT agent_id FROM mailbox WHERE delivered_seq=0"
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+reserved+")").Scan(&count); err != nil {
		return err
	}
	if agent != "" {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+reserved+") WHERE agent_id=?", agent).Scan(&own); err != nil {
			return err
		}
	}
	if own == 0 && count >= maxAgents {
		return protocol.E("limit_exceeded", "active agent capacity reached; existing sessions are preserved")
	}
	return nil
}

func (s *Store) Operation(ctx context.Context, id string) (protocol.Accepted, error) {
	var b string
	if err := s.db.QueryRowContext(ctx, "SELECT result FROM operations WHERE id=?", id).Scan(&b); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, protocol.E("not_found", "operation was not accepted")
		}
		return protocol.Accepted{}, err
	}
	var result protocol.Accepted
	err := json.Unmarshal([]byte(b), &result)
	return result, err
}

func (s *Store) Snapshot(ctx context.Context, id string) (protocol.Snapshot, error) {
	var snap protocol.Snapshot
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		a := &snap.Agent
		err := tx.QueryRowContext(ctx, "SELECT id,coalesce(parent_id,''),cwd,state,model,seq,paused FROM agents WHERE id=?", id).Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq, &a.Paused)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.E("not_found", "agent not found")
		}
		if err != nil {
			return err
		}
		snap.Cursor = a.Seq
		snap.Tasks = []protocol.Task{}
		rows, err := tx.QueryContext(ctx, "SELECT id,agent_id,state,input,result,attachments,skills FROM tasks WHERE agent_id=? ORDER BY rowid", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				return err
			}
			snap.Tasks = append(snap.Tasks, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		questions, err := listQuestions(ctx, tx, protocol.QuestionList{AgentID: id, State: "pending", Limit: protocol.MaxOpenQuestions})
		snap.Questions = questions.Items
		return err
	})
	return snap, err
}

func (s *Store) ListAgents(ctx context.Context) ([]protocol.Agent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,coalesce(parent_id,''),cwd,state,model,seq,paused FROM agents ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []protocol.Agent{}
	for rows.Next() {
		var a protocol.Agent
		if err := rows.Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq, &a.Paused); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) Events(ctx context.Context, c protocol.Cursor) (protocol.EventPage, error) {
	if c.After < 0 || c.Limit < 0 || c.Limit > 1000 {
		return protocol.EventPage{}, protocol.E("invalid_params", "invalid event cursor or limit (maximum 1000)")
	}
	if c.Limit == 0 {
		c.Limit = 100
	}
	p := protocol.EventPage{Events: []protocol.Event{}, Next: c.After}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", c.AgentID).Scan(&p.Latest); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("not_found", "agent not found")
			}
			return err
		}
		if c.After > p.Latest {
			return protocol.E("cursor_gap", "cursor is ahead of durable history; obtain a new snapshot")
		}
		rows, err := tx.QueryContext(ctx, "SELECT seq,kind,task_id,payload FROM events WHERE agent_id=? AND seq>? ORDER BY seq LIMIT ?", c.AgentID, c.After, c.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		pageBytes := 0
		for rows.Next() {
			e := protocol.Event{Version: protocol.Version, AgentID: c.AgentID}
			var payload string
			if err := rows.Scan(&e.Seq, &e.Kind, &e.TaskID, &payload); err != nil {
				return err
			}
			if e.Seq != p.Next+1 {
				return protocol.E("cursor_gap", "durable event range is missing")
			}
			e.Payload = json.RawMessage(payload)
			encoded, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if len(encoded) > protocol.MaxFrameBytes-4096 {
				return protocol.E("output_limit", "event exceeds the IPC budget; original record remains in the profile database")
			}
			if pageBytes+len(encoded) > protocol.MaxFrameBytes-4096 {
				break
			}
			pageBytes += len(encoded) + 1
			p.Events = append(p.Events, e)
			p.Next = e.Seq
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(p.Events) == 0 && p.Next < p.Latest {
			return protocol.E("cursor_gap", "durable event range is missing")
		}
		return nil
	})
	return p, err
}
