package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func sameCopySelection(a, b Selection) bool {
	if !reflect.DeepEqual(a.Choice, b.Choice) {
		return false
	}
	if len(a.Binding) == 0 || len(b.Binding) == 0 {
		return len(a.Binding) == len(b.Binding)
	}
	x, xe := (protocol.Request{Version: 1, Method: "copy-binding", Params: a.Binding}).Fingerprint()
	y, ye := (protocol.Request{Version: 1, Method: "copy-binding", Params: b.Binding}).Fingerprint()
	return xe == nil && ye == nil && x == y
}

func sessionCopyReady(ctx context.Context, tx *sql.Tx, r SessionCopy) (TreeStopRecord, error) {
	plan, err := treeStop(ctx, tx, r.Info.OperationID)
	if err != nil {
		return plan, err
	}
	if plan.State != "stopped" {
		return plan, protocol.E("tree_stopping", "source tree termination is not yet verified")
	}
	if err := treeStopReady(ctx, tx, plan); err != nil {
		return plan, err
	}
	var members, preparing int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM session_copy_members WHERE copy_id=?", r.Info.OperationID).Scan(&members); err != nil {
		return plan, err
	}
	if members != len(plan.Members) {
		return plan, protocol.E("invalid_state", "copy scope differs from its retained tree stop")
	}
	for _, member := range plan.Members {
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT copy_id FROM session_copy_members WHERE agent_id=?", member.AgentID).Scan(&owner); err != nil {
			return plan, err
		}
		if owner != r.Info.OperationID {
			return plan, protocol.E("invalid_state", "copy scope changed")
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM resource_updates u JOIN session_copy_members m ON m.agent_id=u.agent_id
 WHERE m.copy_id=? AND json_extract(u.record,'$.update.state')='preparing'`, r.Info.OperationID).Scan(&preparing); err != nil {
		return plan, err
	}
	if preparing != 0 {
		return plan, protocol.E("copy_preparing", "source resource preparation is still settling")
	}
	return plan, nil
}

func (s *Store) SessionCopyReady(ctx context.Context, id string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := sessionCopy(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Info.State == "completed" {
			return nil
		}
		if _, err = sessionCopyReady(ctx, tx, r); err != nil {
			return err
		}
		choice, err := selection(ctx, tx, r.Info.SourceAgent)
		if err != nil {
			return err
		}
		if !sameCopySelection(choice, r.Selection) {
			return protocol.E("selection_changed", "frozen source selection differs from its copy intent")
		}
		var cursor int64
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", r.Info.SourceAgent).Scan(&cursor); err != nil {
			return err
		}
		entries, err := contextEntries(ctx, tx, r.Info.SourceAgent, cursor)
		if err != nil {
			return err
		}
		complete, err := completePrefix(entries)
		if err != nil {
			return err
		}
		if complete != len(entries) {
			return protocol.E("context_invalid", "source context still has incomplete tool pairs")
		}
		return nil
	})
}

// Publication creates the target and its complete history, independent image
// bytes, resource ownership, view and queue before archiving the source tree.
// All of it rolls back together; no process or old tool effect is dispatched.
func (s *Store) PublishSessionCopy(ctx context.Context, id string, dependency *SpawnDependency) (protocol.SessionCopyInfo, *SpawnDependencyPublication, error) {
	var info protocol.SessionCopyInfo
	var publication *SpawnDependencyPublication
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := sessionCopy(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Info.State == "completed" {
			info = r.Info
			return nil
		}
		plan, err := sessionCopyReady(ctx, tx, r)
		if err != nil {
			return err
		}
		choice, err := selection(ctx, tx, r.Info.SourceAgent)
		if err != nil {
			return err
		}
		if !sameCopySelection(choice, r.Selection) {
			return protocol.E("selection_changed", "frozen source selection differs from its copy intent")
		}
		if err := validateSpawnDependency(ctx, tx, r.Info.SourceAgent, choice, dependency); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", r.Info.SourceAgent).Scan(&r.Info.SourceCursor); err != nil {
			return err
		}
		view, err := contextView(ctx, tx, r.Info.SourceAgent, r.Info.SourceCursor)
		if err != nil {
			return err
		}
		entries, err := contextEntries(ctx, tx, r.Info.SourceAgent, r.Info.SourceCursor)
		if err != nil {
			return err
		}
		complete, err := completePrefix(entries)
		if err != nil {
			return err
		}
		if complete != len(entries) {
			return protocol.E("context_invalid", "source context still has incomplete tool pairs")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agents(id,cwd,state,model,paused) VALUES(?,?,'copying',?,1)", r.Info.AgentID, r.Info.Cwd, choice.Choice.Model); err != nil {
			return err
		}
		if err := copySessionImages(ctx, tx, id, r.Info.SourceAgent, r.Info.AgentID); err != nil {
			return err
		}
		if err := copySessionHistory(ctx, tx, id, r.Info.SourceAgent, r.Info.AgentID, r.Info.SourceCursor); err != nil {
			return err
		}
		selected := ""
		if dependency != nil {
			selected = dependency.Package.Info.ID
		}
		if err := copySessionResources(ctx, tx, r.Info.SourceAgent, r.Info.AgentID, selected); err != nil {
			return err
		}
		created := protocol.Agent{ID: r.Info.AgentID, Cwd: r.Info.Cwd, State: "idle", Model: choice.Choice.Model}
		if _, err := appendEvent(ctx, tx, r.Info.AgentID, "", "agent.created", created); err != nil {
			return err
		}
		next := choice.Choice
		next.AgentID, next.Cwd = r.Info.AgentID, r.Info.Cwd
		if err := tx.QueryRowContext(ctx, "SELECT seq+1 FROM agents WHERE id=?", r.Info.AgentID).Scan(&next.Revision); err != nil {
			return err
		}
		if next.ModelRevision > 0 {
			next.ModelRevision = next.Revision
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agent_selections(agent_id,revision,model_revision,effort,binding) VALUES(?,?,?,?,?)", next.AgentID, next.Revision, next.ModelRevision, next.Effort, string(choice.Binding)); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, next.AgentID, "", "agent.selection", next); err != nil {
			return err
		}
		if dependency != nil {
			publication, err = publishSpawnDependency(ctx, tx, r.Info.AgentID, dependency.Package)
			if err != nil {
				return err
			}
		}
		if view.ID != "" {
			if view.Through <= 0 || view.Through > r.Info.SourceCursor || view.SourceCursor > r.Info.SourceCursor || view.Summary == "" {
				return protocol.E("context_invalid", "source context view cannot be copied")
			}
			view.Previous, view.ID, view.AgentID, view.SourceCursor, view.Published = view.ID, protocol.NewID("view"), r.Info.AgentID, r.Info.SourceCursor, 0
			if _, err := appendEvent(ctx, tx, r.Info.AgentID, "", "context.view", view); err != nil {
				return err
			}
		}
		identity, _ := json.Marshal(struct {
			Source string `json:"source_agent_id"`
			Agent  string `json:"agent_id"`
			Cwd    string `json:"cwd"`
			Cursor int64  `json:"source_cursor"`
		}{r.Info.SourceAgent, r.Info.AgentID, r.Info.Cwd, r.Info.SourceCursor})
		notice := fmt.Sprintf("This is a copied conversation: %s. Source Agent-tree tasks and owned processes have stopped; their IDs are historical references. Continue here under the current system instructions and working directory. Completed effects remain valid. Pending source inputs have new task identities; do not repeat historical tool calls or restart the source Agents.", identity)
		if _, err := appendEvent(ctx, tx, r.Info.AgentID, "", "context.message", ContextMessage{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: notice}}}}); err != nil {
			return err
		}
		if err := copyPendingInputs(ctx, tx, r, plan); err != nil {
			return err
		}
		if err := archiveCopyQueues(ctx, tx, id); err != nil {
			return err
		}
		for _, member := range plan.Members {
			if _, err := tx.ExecContext(ctx, "UPDATE agents SET state='archived',paused=1 WHERE id=?", member.AgentID); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, member.AgentID, "", "agent.archived", map[string]string{"copy_id": id, "copy_agent_id": r.Info.AgentID}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET state='idle',paused=0 WHERE id=? AND state='copying'", r.Info.AgentID); err != nil {
			return err
		}
		r.Info.State, r.Info.Stage, r.Info.Failure = "completed", "completed", nil
		if err := tx.QueryRowContext(ctx, "SELECT seq+1 FROM agents WHERE id=?", r.Info.AgentID).Scan(&r.Info.Cursor); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, r.Info.AgentID, "", "session.copy.completed", r.Info); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, r.Info.SourceAgent, "", "session.copy.completed", r.Info); err != nil {
			return err
		}
		raw, err := json.Marshal(r.Info)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE session_copies SET state='completed',result=? WHERE id=? AND state='stopping'", string(raw), id); err != nil {
			return err
		}
		info = r.Info
		return nil
	})
	if err != nil {
		return protocol.SessionCopyInfo{}, nil, err
	}
	return info, publication, nil
}

// Copies keep immutable source drafts. Completed reads map only their image
// references; source question IDs remain explicit historical identities.
func (s *Store) SessionCopyDrafts(ctx context.Context, id string) (protocol.SessionCopyDrafts, error) {
	return s.ReadSessionCopyDrafts(ctx, protocol.CopyDraftQuery{ID: id})
}

func (s *Store) ReadSessionCopyDrafts(ctx context.Context, query protocol.CopyDraftQuery) (protocol.SessionCopyDrafts, error) {
	var result protocol.SessionCopyDrafts
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := sessionCopy(ctx, tx, query.ID)
		if err != nil {
			return err
		}
		result.Copy = r.Info
		result.Drafts = []protocol.CopyDraft{}
		if r.Info.State != "completed" {
			return protocol.E("copy_pending", "drafts are available after the copy is published")
		}
		drafts := r.Drafts
		if query.Drafts != nil {
			if err := validateCopyDrafts(ctx, tx, r.Info.SourceAgent, query.Drafts); err != nil {
				return err
			}
			drafts = query.Drafts
		}
		for _, draft := range drafts {
			draft.Attachments, err = copiedImageRefs(ctx, tx, query.ID, draft.Attachments)
			if err != nil {
				return err
			}
			if draft.QuestionID != "" && draft.QuestionAgent == "" {
				draft.QuestionAgent = r.Info.SourceAgent
			}
			draft.Skills = append([]protocol.ResourceRef(nil), draft.Skills...)
			result.Drafts = append(result.Drafts, draft)
		}
		return nil
	})
	return result, err
}
