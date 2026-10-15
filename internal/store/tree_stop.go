package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/protocol"
)

const treeStopSchema = `
CREATE INDEX IF NOT EXISTS tree_stop_events ON events(agent_id,kind,seq)
 WHERE kind IN ('agent.tree_stopping','tree_stop.started','tree_stop.completed');
PRAGMA user_version=12;
`

// This bounds atomic scope capture, not the number of active Agents permitted
// by configuration. Exceeding it rejects the whole stop, never a partial tree.
const maxTreeStopMembers = 10000
const maxTreeStopWorkspaces = 10000

type TreeStopMember struct {
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id,omitempty"`
}

// TreeStopPlan is immutable scope captured before any external cancellation.
// A paused Agent is not evidence that its processes/worktree checkout exited.
// Runtime must settle those effects before publishing tree_stop.completed.
type TreeStopPlan struct {
	OperationID string           `json:"operation_id"`
	AgentID     string           `json:"agent_id"`
	Members     []TreeStopMember `json:"members"`
	Worktrees   []string         `json:"worktrees"`
}

type TreeStopRecord struct {
	TreeStopPlan
	State string `json:"state"`
}

const agentTree = `WITH RECURSIVE tree(id) AS (
 SELECT id FROM agents WHERE id=? UNION SELECT a.id FROM agents a JOIN tree t ON a.parent_id=t.id
) `

// One outstanding stop may own an Agent. A single completion on the root
// releases the whole scope; ordinary stop/resume and individual Task exits
// cannot release members early.
func treeWritable(ctx context.Context, q rowQuerier, agent string) error {
	var id string
	err := q.QueryRowContext(ctx, `SELECT json_extract(e.payload,'$.operation_id')
 FROM events e WHERE e.agent_id=? AND e.kind='agent.tree_stopping'
 AND NOT EXISTS(SELECT 1 FROM events done WHERE done.agent_id=json_extract(e.payload,'$.root_agent_id')
 AND done.kind='tree_stop.completed'
 AND json_extract(done.payload,'$.operation_id')=json_extract(e.payload,'$.operation_id'))
 ORDER BY e.seq DESC LIMIT 1`, agent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	failure := protocol.E("tree_stopping", "Agent belongs to an unfinished tree stop; inspect the original operation")
	failure.Data.OperationID = id
	return failure
}

func treeCallerWritable(ctx context.Context, q rowQuerier, caller *protocol.Caller) error {
	if caller == nil {
		return nil
	}
	return treeWritable(ctx, q, caller.AgentID)
}

// BeginTreeStop only freezes dispatch and requests Task cancellation. It does
// not signal processes, stop Git, or claim completion. The entire tree and all
// unaccepted worktree intents are captured in the same transaction as the
// pauses and acceptance receipt. A concurrent spawn is either included or
// rejected; it cannot fall between scope capture and the admission gate.
func (s *Store) BeginTreeStop(ctx context.Context, req protocol.Request, root string) (protocol.Accepted, error) {
	var target struct {
		AgentID string `json:"agent_id"`
		Tree    bool   `json:"tree"`
	}
	if err := protocol.StrictDecode(req.Params, &target); err != nil {
		return protocol.Accepted{}, err
	}
	if req.Method != "agent.stop" || !target.Tree || root == "" || root != target.AgentID {
		return protocol.Accepted{}, protocol.E("invalid_params", "tree stop requires an explicit tree scope matching the target Agent")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		plan := TreeStopPlan{OperationID: req.OperationID, AgentID: root, Members: []TreeStopMember{}, Worktrees: []string{}}
		rows, err := tx.QueryContext(ctx, agentTree+`SELECT a.id,a.state FROM agents a JOIN tree t ON t.id=a.id ORDER BY a.rowid LIMIT ?`, root, maxTreeStopMembers+1)
		if err != nil {
			return protocol.Accepted{}, err
		}
		for rows.Next() {
			var member TreeStopMember
			var state string
			if err := rows.Scan(&member.AgentID, &state); err != nil {
				rows.Close()
				return protocol.Accepted{}, err
			}
			if state == "archived" || state == "copying" {
				rows.Close()
				return protocol.Accepted{}, protocol.E("conflict", "tree contains a read-only Agent")
			}
			plan.Members = append(plan.Members, member)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return protocol.Accepted{}, err
		}
		if len(plan.Members) == 0 {
			return protocol.Accepted{}, protocol.E("not_found", "Agent tree not found")
		}
		if len(plan.Members) > maxTreeStopMembers {
			return protocol.Accepted{}, protocol.E("limit_exceeded", "tree stop exceeds 10000 Agents; no work was stopped")
		}
		rows, err = tx.QueryContext(ctx, agentTree+`
 SELECT w.id FROM workspace_operations w JOIN tree t ON t.id=w.parent_id
 WHERE NOT EXISTS(SELECT 1 FROM operations o WHERE o.id=w.id)
 AND w.state!='cancelled' ORDER BY w.rowid LIMIT ?`, root, maxTreeStopWorkspaces+1)
		if err != nil {
			return protocol.Accepted{}, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return protocol.Accepted{}, err
			}
			plan.Worktrees = append(plan.Worktrees, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return protocol.Accepted{}, err
		}
		if len(plan.Worktrees) > maxTreeStopWorkspaces {
			return protocol.Accepted{}, protocol.E("limit_exceeded", "tree stop exceeds 10000 pending workspaces; no work was stopped")
		}
		for i := range plan.Members {
			member := &plan.Members[i]
			if err := treeWritable(ctx, tx, member.AgentID); err != nil {
				return protocol.Accepted{}, err
			}
			task, err := pauseAgent(ctx, tx, req.OperationID, member.AgentID)
			if err != nil {
				return protocol.Accepted{}, err
			}
			member.TaskID = task
			if _, err := appendEvent(ctx, tx, member.AgentID, task, "agent.tree_stopping", map[string]string{"operation_id": req.OperationID, "root_agent_id": root}); err != nil {
				return protocol.Accepted{}, err
			}
		}
		data, err := json.Marshal(plan)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if len(data) > protocol.MaxFrameBytes-4096 {
			return protocol.Accepted{}, protocol.E("output_limit", "tree stop plan exceeds the IPC budget; no work was stopped")
		}
		if _, err := appendEvent(ctx, tx, root, "", "tree_stop.started", plan); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: root}, nil
	})
}

func (s *Store) TreeStop(ctx context.Context, id string) (TreeStopRecord, error) {
	var result TreeStopRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = treeStop(ctx, tx, id)
		return err
	})
	return result, err
}

func treeStop(ctx context.Context, q rowQuerier, id string) (TreeStopRecord, error) {
	var result TreeStopRecord
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT e.payload FROM operations o
 JOIN events e ON e.agent_id=json_extract(o.result,'$.agent_id')
 WHERE o.id=? AND e.kind='tree_stop.started' AND json_extract(e.payload,'$.operation_id')=o.id`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return result, protocol.E("not_found", "tree stop not found")
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result.TreeStopPlan); err != nil {
		return result, err
	}
	var completed bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE agent_id=? AND kind='tree_stop.completed'
 AND json_extract(payload,'$.operation_id')=?)`, result.AgentID, id).Scan(&completed); err != nil {
		return result, err
	}
	result.State = "stopping"
	if completed {
		result.State = "stopped"
	}
	return result, nil
}
