package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"zende.local/harness/internal/protocol"
)

const worktreeSchema = `
CREATE TABLE workspace_operations (
 id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL,
 parent_id TEXT NOT NULL REFERENCES agents(id), target TEXT NOT NULL UNIQUE,
 request TEXT NOT NULL, plan TEXT NOT NULL, state TEXT NOT NULL,
 failure TEXT NOT NULL DEFAULT 'null'
);
CREATE TRIGGER workspace_intent_immutable BEFORE UPDATE OF id,fingerprint,parent_id,target,request,plan ON workspace_operations
 BEGIN SELECT RAISE(ABORT,'immutable workspace intent'); END;
CREATE TRIGGER workspace_no_delete BEFORE DELETE ON workspace_operations
 BEGIN SELECT RAISE(ABORT,'workspace operation retained'); END;
PRAGMA user_version=9;
`

type WorktreeRecord struct {
	Operation protocol.WorktreeOperation
	Request   protocol.Request
	Params    protocol.SpawnAgent
}

func (s *Store) WorktreeLockPath(id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(filepath.Dir(s.path), "workspace-locks", hex.EncodeToString(hash[:])+".lock")
}

func worktreeFingerprint(ctx context.Context, q rowQuerier, id, want string) error {
	var have string
	err := q.QueryRowContext(ctx, "SELECT fingerprint FROM workspace_operations WHERE id=?", id).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if have != want {
		return protocol.E("operation_conflict", "operation_id is reserved for a different workspace request")
	}
	return nil
}

func worktreeRecord(ctx context.Context, q rowQuerier, id string) (*WorktreeRecord, error) {
	r := &WorktreeRecord{}
	var request, plan, failure []byte
	err := q.QueryRowContext(ctx, "SELECT parent_id,request,plan,state,failure FROM workspace_operations WHERE id=?", id).Scan(&r.Operation.ParentID, &request, &plan, &r.Operation.State, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Operation.OperationID = id
	if err := json.Unmarshal(request, &r.Request); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(r.Request.Params, &r.Params); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plan, &r.Operation.Workspace); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(failure, &r.Operation.Failure); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) WorktreeRecord(ctx context.Context, req protocol.Request) (*WorktreeRecord, error) {
	fingerprint, err := req.EffectFingerprint()
	if err != nil {
		return nil, err
	}
	if err := worktreeFingerprint(ctx, s.db, req.OperationID, fingerprint); err != nil {
		return nil, err
	}
	return worktreeRecord(ctx, s.db, req.OperationID)
}

func (s *Store) WorktreeOperation(ctx context.Context, id string) (*protocol.WorktreeOperation, error) {
	r, err := worktreeRecord(ctx, s.db, id)
	if err != nil || r == nil {
		return nil, err
	}
	return &r.Operation, nil
}

// Validate the same context/caller/limits as Spawn before touching Git. Final
// acceptance rechecks them: changes during checkout may leave a retained ready
// workspace, but cannot publish an invalid or over-capacity child.
func (s *Store) PrepareWorktree(ctx context.Context, req protocol.Request, p protocol.SpawnAgent, plan protocol.Worktree, revision int64, maxAgents, maxDepth int) (*WorktreeRecord, error) {
	var result *WorktreeRecord
	fingerprint, err := req.EffectFingerprint()
	if err != nil {
		return nil, err
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if req.OperationID == "" || p.Workspace != "worktree" || !filepath.IsAbs(plan.Path) || !filepath.IsAbs(plan.Repository) || plan.Owner == "" || plan.Commit == "" {
			return protocol.E("invalid_params", "invalid worktree preparation")
		}
		if err := worktreeFingerprint(ctx, tx, req.OperationID, fingerprint); err != nil {
			return err
		}
		old, err := worktreeRecord(ctx, tx, req.OperationID)
		if err != nil {
			return err
		}
		if old != nil {
			result = old
			return nil
		}
		var acceptedHash string
		err = tx.QueryRowContext(ctx, "SELECT fingerprint FROM operations WHERE id=?", req.OperationID).Scan(&acceptedHash)
		if err == nil {
			return protocol.E("operation_conflict", "operation_id was already accepted; inspect its original result")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, _, _, err := prepareSpawn(ctx, tx, p, plan.Path, revision, maxAgents, maxDepth); err != nil {
			return err
		}
		var occupied string
		err = tx.QueryRowContext(ctx, "SELECT id FROM workspace_operations WHERE target=?", plan.Path).Scan(&occupied)
		if err == nil {
			return protocol.E("worktree_reserved", "target is reserved by another operation; inspect that operation before reusing the directory")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		requestData, err := json.Marshal(req)
		if err != nil {
			return err
		}
		planData, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO workspace_operations(id,fingerprint,parent_id,target,request,plan,state) VALUES(?,?,?,?,?,?,'prepared')", req.OperationID, fingerprint, p.ParentID, plan.Path, string(requestData), string(planData)); err != nil {
			return err
		}
		result = &WorktreeRecord{Operation: protocol.WorktreeOperation{OperationID: req.OperationID, ParentID: p.ParentID, State: "prepared", Workspace: plan}, Request: req, Params: p}
		_, err = appendEvent(ctx, tx, p.ParentID, "", "workspace.prepared", result.Operation)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) TransitionWorktree(ctx context.Context, req protocol.Request, previous, next string, failure *protocol.Error) (*WorktreeRecord, error) {
	var result *WorktreeRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		fingerprint, err := req.EffectFingerprint()
		if err != nil {
			return err
		}
		if err := worktreeFingerprint(ctx, tx, req.OperationID, fingerprint); err != nil {
			return err
		}
		r, err := worktreeRecord(ctx, tx, req.OperationID)
		if err != nil {
			return err
		}
		if r == nil || r.Operation.State != previous {
			return protocol.E("worktree_conflict", "workspace state changed; inspect the original operation")
		}
		valid := previous == "prepared" && next == "creating" || (previous == "creating" || previous == "unknown") && (next == "ready" || next == "unknown")
		if !valid {
			return protocol.E("invalid_params", "invalid workspace transition")
		}
		if next == "creating" {
			if err := treeWritable(ctx, tx, r.Operation.ParentID); err != nil {
				return err
			}
		}
		r.Operation.State, r.Operation.Failure = next, failure
		data, err := json.Marshal(failure)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE workspace_operations SET state=?,failure=? WHERE id=?", next, string(data), req.OperationID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, r.Operation.ParentID, "", "workspace."+next, r.Operation); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}
