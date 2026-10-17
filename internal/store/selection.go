package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const selectionSchema = `
CREATE TABLE agent_selections (
 agent_id TEXT PRIMARY KEY REFERENCES agents(id), revision INTEGER NOT NULL,
 model_revision INTEGER NOT NULL, effort TEXT NOT NULL, binding TEXT NOT NULL
);
PRAGMA user_version=7;
`

// Binding is a provider snapshot with credential references, not resolved keys.
// It is private to the runtime; the ordinary selection API returns only Choice.
type Selection struct {
	Choice  protocol.AgentSelection
	Binding json.RawMessage
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func selection(ctx context.Context, q rowQuerier, id string) (Selection, error) {
	var s Selection
	var raw string
	err := q.QueryRowContext(ctx, `SELECT a.id,a.cwd,a.model,
 coalesce(s.revision,0),coalesce(s.model_revision,0),coalesce(s.effort,''),coalesce(s.binding,'')
 FROM agents a LEFT JOIN agent_selections s ON a.id=s.agent_id WHERE a.id=?`, id).Scan(&s.Choice.AgentID, &s.Choice.Cwd, &s.Choice.Model, &s.Choice.Revision, &s.Choice.ModelRevision, &s.Choice.Effort, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Selection{}, protocol.E("not_found", "agent not found")
	}
	s.Binding = json.RawMessage(raw)
	return s, err
}

func (s *Store) Selection(ctx context.Context, agent string) (Selection, error) {
	return selection(ctx, s.db, agent)
}

// Select commits the desired settings, append-only event and acceptance
// together. The caller prepared against previous; concurrent choices conflict.
func (s *Store) Select(ctx context.Context, req protocol.Request, next Selection, previous int64, modelChanged bool) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := treeWritable(ctx, tx, next.Choice.AgentID); err != nil {
			return protocol.Accepted{}, err
		}
		if !filepath.IsAbs(next.Choice.Cwd) || previous < 0 || len(next.Binding) > model.MaxBlockBytes || modelChanged && (next.Choice.Model == "" || !json.Valid(next.Binding)) {
			return protocol.Accepted{}, protocol.E("invalid_params", "invalid prepared selection")
		}
		old, err := selection(ctx, tx, next.Choice.AgentID)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if old.Choice.Revision != previous {
			return protocol.Accepted{}, protocol.E("selection_changed", "Agent settings changed; inspect and select again")
		}
		var state string
		var seq int64
		if err := tx.QueryRowContext(ctx, "SELECT state,seq FROM agents WHERE id=?", next.Choice.AgentID).Scan(&state, &seq); err != nil {
			return protocol.Accepted{}, err
		}
		if state == "copying" || state == "archived" {
			return protocol.Accepted{}, protocol.E("conflict", "agent is read-only")
		}
		next.Choice.Revision = seq + 1
		if modelChanged {
			next.Choice.ModelRevision = next.Choice.Revision
		} else {
			next.Choice.Model, next.Choice.Effort, next.Choice.ModelRevision, next.Binding = old.Choice.Model, old.Choice.Effort, old.Choice.ModelRevision, old.Binding
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET cwd=?,model=? WHERE id=?", next.Choice.Cwd, next.Choice.Model, next.Choice.AgentID); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_selections(agent_id,revision,model_revision,effort,binding) VALUES(?,?,?,?,?)
 ON CONFLICT(agent_id) DO UPDATE SET revision=excluded.revision,model_revision=excluded.model_revision,effort=excluded.effort,binding=excluded.binding`, next.Choice.AgentID, next.Choice.Revision, next.Choice.ModelRevision, next.Choice.Effort, string(next.Binding)); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, next.Choice.AgentID, "", "agent.selection", next.Choice); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: next.Choice.AgentID, Selection: &next.Choice}, nil
	})
}

// BeginModelRequest fences against stop and concurrent context/selection
// changes. A new runtime notice and model.started are one commit, so recovery
// cannot see new settings attached to the previous request's pending calls.
func (s *Store) BeginModelRequest(ctx context.Context, agent, task string, revision, cursor int64, notices []ContextMessage, record func(int64) any) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		seq, err := requestBoundary(ctx, tx, agent, task, revision, cursor)
		if err != nil {
			return err
		}
		var compacting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM compactions WHERE agent_id=? AND json_extract(record,'$.state')='running')`, agent).Scan(&compacting); err != nil {
			return err
		}
		if compacting {
			return protocol.E("context_busy", "wait for the active summary attempt before another model request")
		}
		if err := manualCompactionBoundary(ctx, tx, agent); err != nil {
			return err
		}
		for _, notice := range notices {
			if notice.Message.Role != "developer" || notice.RequestID != "" || notice.MessageID != "" || len(notice.MessageIDs) != 0 || notice.QuestionID != "" || notice.Summary != nil {
				return protocol.E("invalid_params", "request notices must be developer context, not model or tool results")
			}
			event, err := appendEvent(ctx, tx, agent, task, "context.message", notice)
			if err != nil {
				return err
			}
			seq = event.Seq
		}
		_, err = appendEvent(ctx, tx, agent, task, "model.started", record(seq))
		return err
	})
}

// Both dispatch and a capacity refusal must refer to the still-current input.
// A concurrent selection, publication or steer requires rebuilding either.
func requestBoundary(ctx context.Context, tx *sql.Tx, agent, task string, revision, cursor int64) (int64, error) {
	if err := activeTask(ctx, tx, agent, task); err != nil {
		return 0, err
	}
	current, err := selection(ctx, tx, agent)
	if err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", agent).Scan(&seq); err != nil {
		return 0, err
	}
	if current.Choice.Revision != revision || seq != cursor {
		return 0, protocol.E("request_conflict", "context or settings changed before request start; rebuild the request")
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailbox m WHERE m.agent_id=? AND "+readySteer+")", agent).Scan(&pending); err != nil {
		return 0, err
	}
	if pending {
		return 0, protocol.E("request_conflict", "steer arrived before request start; deliver before rebuilding")
	}
	return seq, nil
}
