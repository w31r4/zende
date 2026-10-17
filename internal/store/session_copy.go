package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const sessionCopySchema = `
CREATE TABLE session_copies (
 id TEXT PRIMARY KEY, source_agent TEXT NOT NULL REFERENCES agents(id),
 target_agent TEXT NOT NULL UNIQUE, cwd TEXT NOT NULL, plan TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('stopping','completed')), result TEXT NOT NULL DEFAULT ''
);
CREATE TABLE session_copy_members (
 agent_id TEXT PRIMARY KEY REFERENCES agents(id), copy_id TEXT NOT NULL REFERENCES session_copies(id)
);
CREATE INDEX copy_members_operation ON session_copy_members(copy_id);
CREATE TABLE session_copy_images (
 copy_id TEXT NOT NULL REFERENCES session_copies(id), source_id TEXT NOT NULL REFERENCES attachments(id),
 target_id TEXT NOT NULL UNIQUE REFERENCES attachments(id), PRIMARY KEY(copy_id,source_id)
);
CREATE TRIGGER session_copy_identity BEFORE UPDATE OF id,source_agent,target_agent,cwd,plan ON session_copies
 BEGIN SELECT RAISE(ABORT,'immutable copy intent'); END;
CREATE TRIGGER session_copy_terminal BEFORE UPDATE ON session_copies WHEN OLD.state='completed'
 BEGIN SELECT RAISE(ABORT,'immutable completed copy'); END;
CREATE TRIGGER session_copy_retained BEFORE DELETE ON session_copies
 BEGIN SELECT RAISE(ABORT,'retained copy'); END;
CREATE TRIGGER session_copy_member_identity BEFORE UPDATE ON session_copy_members
 BEGIN SELECT RAISE(ABORT,'immutable copy scope'); END;
CREATE TRIGGER session_copy_member_retained BEFORE DELETE ON session_copy_members
 BEGIN SELECT RAISE(ABORT,'retained copy scope'); END;
CREATE TRIGGER session_copy_image_identity BEFORE UPDATE ON session_copy_images
 BEGIN SELECT RAISE(ABORT,'immutable copied image'); END;
CREATE TRIGGER session_copy_image_retained BEFORE DELETE ON session_copy_images
 BEGIN SELECT RAISE(ABORT,'retained copied image'); END;
ALTER TABLE events ADD COLUMN source_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN source_seq INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=23;
`

type sessionCopyPlan struct {
	Version   int                     `json:"version"`
	Selection protocol.AgentSelection `json:"selection"`
	Binding   json.RawMessage         `json:"binding,omitempty"`
	Drafts    []protocol.CopyDraft    `json:"drafts,omitempty"`
}

type SessionCopy struct {
	Info      protocol.SessionCopyInfo
	Selection Selection
	Drafts    []protocol.CopyDraft
}

// The fence outlives tree_stop.completed. Only atomic publication can archive
// the source and release a new independent Agent to the scheduler.
func copyWritable(ctx context.Context, q rowQuerier, agent string) error {
	var id, state string
	err := q.QueryRowContext(ctx, `SELECT c.id,c.state FROM session_copy_members m
 JOIN session_copies c ON c.id=m.copy_id WHERE m.agent_id=?`, agent).Scan(&id, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	failure := protocol.E("copy_pending", "Agent belongs to a retained session copy; inspect the original copy operation")
	if state == "completed" {
		failure = protocol.E("session_archived", "source session is read-only; continue in its published copy")
	}
	failure.Data.OperationID = id
	return failure
}

func validateCopyDrafts(ctx context.Context, tx *sql.Tx, agent string, drafts []protocol.CopyDraft) error {
	if len(drafts) > protocol.MaxCopyDrafts {
		return protocol.E("invalid_params", "copy retains at most 17 input drafts")
	}
	ids, questions := map[string]bool{}, map[string]bool{}
	bytes := 0
	for _, draft := range drafts {
		if draft.ID == "" || len(draft.ID) > 128 || !utf8.ValidString(draft.ID) || strings.ContainsRune(draft.ID, 0) || ids[draft.ID] || questions[draft.QuestionID] || len(draft.QuestionID) > 128 || !utf8.ValidString(draft.QuestionID) || strings.ContainsRune(draft.QuestionID, 0) || !utf8.ValidString(draft.Text) || len(draft.Text) > 6<<20 {
			return protocol.E("invalid_params", "copy draft identity or text is invalid")
		}
		ids[draft.ID], questions[draft.QuestionID] = true, true
		if draft.QuestionID != "" {
			var found bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM questions WHERE agent_id=? AND id=?)", agent, draft.QuestionID).Scan(&found); err != nil {
				return err
			}
			if !found {
				return protocol.E("not_found", "answer draft does not belong to the source session")
			}
		}
		if err := validateAttachments(ctx, tx, agent, draft.Attachments); err != nil {
			return err
		}
		if _, err := selectedSkills(ctx, tx, agent, draft.Skills); err != nil {
			return err
		}
		raw, err := json.Marshal(draft)
		if err != nil {
			return protocol.E("invalid_params", "copy draft cannot be encoded")
		}
		bytes += len(raw)
		if bytes > protocol.MaxCopyDraftBytes {
			return protocol.E("input_limit", "copy drafts exceed the 6 MiB encoded input budget; source and drafts are retained")
		}
	}
	return nil
}

// Intent, drafts, selection, stop scope and acceptance commit together. No
// target Agent is published or dispatched by accepting this operation.
func (s *Store) BeginSessionCopy(ctx context.Context, req protocol.Request, cwd string, revision int64) (protocol.Accepted, error) {
	var p protocol.CopyAgent
	if err := protocol.StrictDecode(req.Params, &p); err != nil {
		return protocol.Accepted{}, err
	}
	if req.Method != "agent.copy" || !p.Confirm || p.AgentID == "" || !filepath.IsAbs(cwd) || revision < 0 {
		return protocol.Accepted{}, protocol.E("invalid_params", "session copy requires explicit confirmation, a source and a prepared directory")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		choice, err := selection(ctx, tx, p.AgentID)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if choice.Choice.Revision != revision || p.ExpectedRevision != nil && *p.ExpectedRevision != revision {
			return protocol.Accepted{}, protocol.E("selection_changed", "source selection changed; inspect and confirm copying again")
		}
		if err := validateCopyDrafts(ctx, tx, p.AgentID, p.Drafts); err != nil {
			return protocol.Accepted{}, err
		}
		accepted, err := beginTreeStop(ctx, tx, req.OperationID, p.AgentID)
		if err != nil {
			return protocol.Accepted{}, err
		}
		info := protocol.SessionCopyInfo{OperationID: req.OperationID, SourceAgent: p.AgentID, AgentID: protocol.NewID("agent"), Cwd: cwd, State: "stopping", Stage: "processes", Drafts: len(p.Drafts)}
		plan := sessionCopyPlan{Version: 1, Selection: choice.Choice, Binding: choice.Binding, Drafts: p.Drafts}
		raw, err := json.Marshal(plan)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if len(raw) > protocol.MaxFrameBytes-4096 {
			return protocol.Accepted{}, protocol.E("input_limit", "copy intent exceeds the persistence budget; source is retained")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO session_copies(id,source_agent,target_agent,cwd,plan,state) VALUES(?,?,?,?,?,'stopping')", req.OperationID, p.AgentID, info.AgentID, cwd, string(raw)); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_copy_members(agent_id,copy_id)
 SELECT agent_id,? FROM events WHERE kind='agent.tree_stopping' AND json_extract(payload,'$.operation_id')=? AND source_agent=''`, req.OperationID, req.OperationID); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, p.AgentID, "", "session.copy.started", info); err != nil {
			return protocol.Accepted{}, err
		}
		accepted.Copy = &info
		return accepted, nil
	})
}
