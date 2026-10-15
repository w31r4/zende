package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// Spawn writes identity, provenance, inherited context, settings, first Task
// and the retry receipt together. cwd was checked by core against revision.
func (s *Store) Spawn(ctx context.Context, req protocol.Request, p protocol.SpawnAgent, cwd string, revision int64, maxAgents, maxDepth int) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if p.ParentID == "" || strings.TrimSpace(p.Input) == "" || len(p.Input) > 2<<20 || !utf8.ValidString(p.Input) || !filepath.IsAbs(cwd) {
			return protocol.Accepted{}, protocol.E("invalid_params", "parent, task input (up to 2 MiB) and an absolute cwd are required")
		}
		settings, err := selection(ctx, tx, p.ParentID)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if settings.Choice.Revision != revision {
			return protocol.Accepted{}, protocol.E("selection_changed", "parent settings changed; retry spawn")
		}
		var state string
		var cursor int64
		if err := tx.QueryRowContext(ctx, "SELECT state,seq FROM agents WHERE id=?", p.ParentID).Scan(&state, &cursor); err != nil {
			return protocol.Accepted{}, err
		}
		if state == "copying" || state == "archived" {
			return protocol.Accepted{}, protocol.E("conflict", "parent is read-only")
		}
		source := protocol.SpawnSource{ParentID: p.ParentID, ParentCursor: cursor, Caller: p.Caller}
		if err := spawnCaller(ctx, tx, &source); err != nil {
			return protocol.Accepted{}, err
		}
		// Root is depth zero; each spawn adds one edge. No filesystem or process
		// work occurs before capacity and ancestry checks have succeeded.
		ancestor := p.ParentID
		for depth := 1; ancestor != ""; depth++ {
			if depth > maxDepth {
				return protocol.Accepted{}, protocol.E("limit_exceeded", "agent recursion depth reached")
			}
			if err := tx.QueryRowContext(ctx, "SELECT coalesce(parent_id,'') FROM agents WHERE id=?", ancestor).Scan(&ancestor); err != nil {
				return protocol.Accepted{}, err
			}
		}
		if err := checkCapacity(ctx, tx, "", maxAgents); err != nil {
			return protocol.Accepted{}, err
		}
		entries, err := spawnContext(ctx, tx, p, &source)
		if err != nil {
			return protocol.Accepted{}, err
		}
		child := protocol.Agent{ID: protocol.NewID("agent"), ParentID: p.ParentID, Cwd: cwd, State: "idle", Model: settings.Choice.Model}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agents(id,parent_id,cwd,state,model) VALUES(?,?,?,?,?)", child.ID, child.ParentID, child.Cwd, child.State, child.Model); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, child.ID, "", "agent.created", child); err != nil {
			return protocol.Accepted{}, err
		}
		choice := settings.Choice
		choice.AgentID, choice.Cwd, choice.Revision = child.ID, cwd, 2
		if choice.ModelRevision > 0 {
			choice.ModelRevision = 2
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO agent_selections(agent_id,revision,model_revision,effort,binding) VALUES(?,?,?,?,?)", child.ID, choice.Revision, choice.ModelRevision, choice.Effort, string(settings.Binding)); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, child.ID, "", "agent.selection", choice); err != nil {
			return protocol.Accepted{}, err
		}
		for _, entry := range entries {
			record := entry.Record
			// These are historical messages, not the child's live requests/questions.
			record.RequestID, record.QuestionID = "", ""
			for i := range record.Message.Content {
				block := &record.Message.Content[i]
				if block.Type != "image" {
					continue
				}
				original := block.Attachment
				if source.Images == nil {
					source.Images = map[string]string{}
				}
				copied := source.Images[original]
				if copied == "" {
					copied = protocol.NewID("image")
					result, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,agent_id,name,media_type,sha256,bytes,width,height,data)
 SELECT ?,?,name,media_type,sha256,bytes,width,height,data FROM attachments WHERE agent_id=? AND id=?`, copied, child.ID, entry.AgentID, original)
					if err != nil {
						return protocol.Accepted{}, err
					}
					n, err := result.RowsAffected()
					if err != nil {
						return protocol.Accepted{}, err
					}
					if n != 1 {
						return protocol.Accepted{}, protocol.E("context_invalid", "inherited image is missing")
					}
					source.Images[original] = copied
				}
				block.Attachment = copied
			}
			if _, err := appendEvent(ctx, tx, child.ID, "", "context.message", record); err != nil {
				return protocol.Accepted{}, err
			}
		}
		if _, err := appendEvent(ctx, tx, child.ID, "", "context.inherited", source); err != nil {
			return protocol.Accepted{}, err
		}
		identity, _ := json.Marshal(struct {
			AgentID  string `json:"agent_id"`
			ParentID string `json:"parent_id"`
			Cwd      string `json:"cwd"`
		}{child.ID, p.ParentID, cwd})
		notice := fmt.Sprintf("You are a new Agent: %s. Earlier messages are inherited background, not live tasks or pending tool calls. Your current system instructions and this identity supersede inherited runtime settings. Work on the task input that follows; do not resume the parent's unfinished work.", identity)
		if _, err := appendEvent(ctx, tx, child.ID, "", "context.message", ContextMessage{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: notice}}}}); err != nil {
			return protocol.Accepted{}, err
		}
		task, err := createTask(ctx, tx, child.ID, p.Input)
		if err != nil {
			return protocol.Accepted{}, err
		}
		accepted := protocol.Accepted{OperationID: req.OperationID, State: "accepted", AgentID: child.ID, TaskID: task.ID, Spawn: &source}
		if _, err := appendEvent(ctx, tx, p.ParentID, "", "agent.spawned", struct {
			OperationID string            `json:"operation_id"`
			Child       protocol.Accepted `json:"child"`
		}{req.OperationID, accepted}); err != nil {
			return protocol.Accepted{}, err
		}
		return accepted, nil
	})
}

func spawnCaller(ctx context.Context, tx *sql.Tx, source *protocol.SpawnSource) error {
	caller := source.Caller
	if caller == nil {
		return nil
	} // explicit human --parent
	if caller.AgentID != source.ParentID {
		return protocol.E("invalid_params", "caller must identify a tool dispatch from the parent")
	}
	origin, err := callSource(ctx, tx, caller)
	if err != nil {
		return err
	}
	source.RequestID, source.CallID = origin.RequestID, origin.CallID
	return nil
}

func callSource(ctx context.Context, tx *sql.Tx, caller *protocol.Caller) (*protocol.CallSource, error) {
	if caller == nil {
		return nil, nil
	}
	if caller.AgentID == "" || caller.TaskID == "" || caller.OperationID == "" {
		return nil, protocol.E("invalid_params", "caller must identify a tool dispatch")
	}
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND task_id=? AND kind='tool.dispatching'
 AND json_extract(payload,'$.operation_id')=? ORDER BY seq LIMIT 1`, caller.AgentID, caller.TaskID, caller.OperationID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, protocol.E("caller_unknown", "caller tool dispatch was not recorded")
	}
	if err != nil {
		return nil, err
	}
	var dispatch struct {
		RequestID string         `json:"request_id"`
		Call      model.ToolCall `json:"call"`
	}
	if err := json.Unmarshal(data, &dispatch); err != nil {
		return nil, err
	}
	if dispatch.RequestID == "" || dispatch.Call.ID == "" || dispatch.Call.Name != "exec_command" {
		return nil, protocol.E("caller_unknown", "caller does not identify command execution")
	}
	return &protocol.CallSource{Caller: *caller, RequestID: dispatch.RequestID, CallID: dispatch.Call.ID}, nil
}

// Only a complete prefix can be inherited. In particular, a spawn invoked
// inside exec_command must not copy its own unfinished call into the child.
// Preserve complete messages, including provider-private fields, verbatim.
func completePrefix(entries []contextEntry) (int, error) {
	complete, _, _, err := scanContext(entries)
	return complete, err
}
