package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

type worktreeProcess struct {
	OperationID string        `json:"operation_id"`
	Group       process.Group `json:"group"`
}

// WorktreeStopRecord holds kernel evidence, not a Git success receipt. A stopped
// group is insufficient to cancel: runtime must also acquire the inherited lock.
type WorktreeStopRecord struct {
	OperationID string         `json:"operation_id"`
	WorktreeID  string         `json:"worktree_id"`
	Group       *process.Group `json:"group,omitempty"`
	Stopped     bool           `json:"stopped"`
}

func validWorktreeGroup(g process.Group) bool {
	valid := func(pid int, identity string, optional bool) bool {
		if pid <= 1 || pid > 1<<31-1 || len(identity) > 128 {
			return false
		}
		if optional && identity == "" {
			return true
		}
		boot, start, ok := strings.Cut(identity, ":")
		return ok && boot != "" && start != "" && strings.IndexFunc(start, func(r rune) bool { return r < '0' || r > '9' }) < 0
	}
	if !valid(g.PID, g.Identity, true) || len(g.Witnesses) > 10000 {
		return false
	}
	seen := map[int]bool{}
	for _, w := range g.Witnesses {
		if !valid(w.PID, w.Identity, false) || seen[w.PID] {
			return false
		}
		seen[w.PID] = true
	}
	return true
}

func worktreeProcessGroup(ctx context.Context, q rowQuerier, r *WorktreeRecord) (*process.Group, error) {
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='workspace.process'
 AND json_extract(payload,'$.operation_id')=? ORDER BY seq LIMIT 1`, r.Operation.ParentID, r.Operation.OperationID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved worktreeProcess
	if err := protocol.StrictDecode(data, &saved); err != nil {
		return nil, err
	}
	if !validWorktreeGroup(saved.Group) || len(saved.Group.Witnesses) != 0 {
		return nil, protocol.E("invalid_state", "saved workspace process identity is invalid")
	}
	return &saved.Group, nil
}

// This may arrive after BeginTreeStop: checkout was admitted before the fence.
// Runtime holds the operation lock through Start, this write, Wait and settlement.
func (s *Store) RecordWorktreeProcess(ctx context.Context, req protocol.Request, g process.Group) error {
	if !validWorktreeGroup(g) || len(g.Witnesses) != 0 {
		return protocol.E("invalid_params", "invalid original workspace process identity")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
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
		if r == nil {
			return protocol.E("not_found", "workspace operation not found")
		}
		old, err := worktreeProcessGroup(ctx, tx, r)
		if err != nil {
			return err
		}
		if old != nil {
			if old.PID != g.PID || old.Identity != g.Identity {
				return protocol.E("operation_conflict", "workspace process identity is immutable")
			}
			return nil
		}
		if r.Operation.State != "creating" && r.Operation.State != "unknown" {
			return protocol.E("worktree_conflict", "workspace is not creating; no process identity was recorded")
		}
		_, err = appendEvent(ctx, tx, r.Operation.ParentID, "", "workspace.process", worktreeProcess{req.OperationID, g})
		return err
	})
}

func scopedWorktree(ctx context.Context, q rowQuerier, stop, id string) (*WorktreeRecord, error) {
	plan, err := treeStop(ctx, q, stop)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(plan.Worktrees, id) {
		return nil, protocol.E("operation_conflict", "workspace is not in the captured stop scope")
	}
	r, err := worktreeRecord(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, protocol.E("invalid_state", "captured workspace operation is missing")
	}
	switch r.Operation.State {
	case "prepared", "creating", "unknown", "ready", "cancelled":
	default:
		return nil, protocol.E("invalid_state", "captured workspace state is invalid")
	}
	var accepted bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE id=?)", id).Scan(&accepted); err != nil {
		return nil, err
	}
	if accepted {
		return nil, protocol.E("operation_conflict", "accepted workspace cannot be cancelled")
	}
	return r, nil
}

func worktreeStop(ctx context.Context, q rowQuerier, stop string, r *WorktreeRecord) (WorktreeStopRecord, error) {
	record := WorktreeStopRecord{OperationID: stop, WorktreeID: r.Operation.OperationID}
	group, err := worktreeProcessGroup(ctx, q, r)
	if err != nil {
		return record, err
	}
	record.Group = group
	var data []byte
	err = q.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='workspace.stop_observed'
 AND json_extract(payload,'$.operation_id')=? AND json_extract(payload,'$.worktree_id')=? ORDER BY seq DESC LIMIT 1`, r.Operation.ParentID, stop, record.WorktreeID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	var saved WorktreeStopRecord
	if err := protocol.StrictDecode(data, &saved); err != nil {
		return record, err
	}
	if saved.OperationID != stop || saved.WorktreeID != record.WorktreeID || saved.Group == nil || group == nil || !validWorktreeGroup(*saved.Group) || saved.Group.PID != group.PID || saved.Group.Identity != group.Identity {
		return record, protocol.E("invalid_state", "workspace stop evidence differs from the original process")
	}
	return saved, nil
}

func (s *Store) WorktreeStop(ctx context.Context, stop, id string) (WorktreeStopRecord, error) {
	var result WorktreeStopRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := scopedWorktree(ctx, tx, stop, id)
		if err != nil {
			return err
		}
		result, err = worktreeStop(ctx, tx, stop, r)
		return err
	})
	return result, err
}

// Repeated observations do not grow history; new evidence is append-only.
func (s *Store) RecordWorktreeStop(ctx context.Context, record WorktreeStopRecord) error {
	if record.Group == nil || !validWorktreeGroup(*record.Group) {
		return protocol.E("invalid_params", "workspace stop requires process evidence")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := scopedWorktree(ctx, tx, record.OperationID, record.WorktreeID)
		if err != nil {
			return err
		}
		old, err := worktreeStop(ctx, tx, record.OperationID, r)
		if err != nil {
			return err
		}
		if old.Group == nil || old.Group.PID != record.Group.PID || old.Group.Identity != record.Group.Identity {
			return protocol.E("operation_conflict", "workspace stop cannot replace the original process")
		}
		if reflect.DeepEqual(old, record) {
			return nil
		}
		if old.Stopped || r.Operation.State == "cancelled" {
			return protocol.E("worktree_conflict", "workspace stop is already settled")
		}
		_, err = appendEvent(ctx, tx, r.Operation.ParentID, "", "workspace.stop_observed", record)
		return err
	})
}

// Caller must hold the original operation lock. That excludes an in-flight
// spawn handler and surviving Git/filter descriptors, including the Start-to-
// identity-write crash window. This method never deletes checkout files.
func (s *Store) CancelWorktree(ctx context.Context, stop, id string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := scopedWorktree(ctx, tx, stop, id)
		if err != nil {
			return err
		}
		if r.Operation.State == "cancelled" {
			return nil
		}
		evidence, err := worktreeStop(ctx, tx, stop, r)
		if err != nil {
			return err
		}
		if evidence.Group != nil && !evidence.Stopped {
			return protocol.E("worktree_busy", "workspace process termination is not verified")
		}
		r.Operation.State = "cancelled"
		r.Operation.Failure = protocol.E("cancelled", "workspace creation cancelled by tree stop; existing files are preserved")
		r.Operation.Failure.Data.OperationID = stop
		failure, err := json.Marshal(r.Operation.Failure)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE workspace_operations SET state='cancelled',failure=? WHERE id=?", string(failure), id); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, r.Operation.ParentID, "", "workspace.cancelled", r.Operation)
		return err
	})
}
