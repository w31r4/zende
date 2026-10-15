package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func worktreeStopFixture(t *testing.T, s *Store) (protocol.Request, protocol.Request, protocol.SpawnAgent) {
	t.Helper()
	ctx := context.Background()
	req, p, plan := prepareWorkspaceFixture(t, s)
	if _, err := s.PrepareWorktree(ctx, req, p, plan, 0, 8, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
		t.Fatal(err)
	}
	stop := treeRequest(p.ParentID, "stop-worktree")
	if _, err := s.BeginTreeStop(ctx, stop, p.ParentID); err != nil {
		t.Fatal(err)
	}
	return req, stop, p
}

func TestWorktreeStopIdentityEvidenceAndCancellation(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	req, stop, p := worktreeStopFixture(t, s)
	group := process.Group{PID: 12345, Identity: "boot-fixture:123"}
	// Start was already admitted, so its late identity must survive the fence.
	if err := s.RecordWorktreeProcess(ctx, req, group); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx, p.ParentID)
	if err := s.RecordWorktreeProcess(ctx, req, group); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Snapshot(ctx, p.ParentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate identity appended history")
	}
	changed := group
	changed.Identity = "reused:456"
	if err := s.RecordWorktreeProcess(ctx, req, changed); err == nil {
		t.Fatal("original PID lifetime changed")
	}
	conflict, _ := protocol.NewRequest("agent.spawn", req.OperationID, protocol.SpawnAgent{ParentID: p.ParentID, Input: "different"})
	if err := s.RecordWorktreeProcess(ctx, conflict, group); err == nil {
		t.Fatal("different request reused identity")
	}
	record, err := s.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || record.Group == nil || !reflect.DeepEqual(*record.Group, group) || record.Stopped {
		t.Fatal(record, err)
	}
	if err := s.CancelWorktree(ctx, stop.OperationID, req.OperationID); err == nil {
		t.Fatal("unverified group treated as stopped")
	}
	record.Group.Witnesses = []process.GroupMember{{PID: 12346, Identity: "boot-fixture:124"}}
	if err := s.RecordWorktreeStop(ctx, record); err != nil {
		t.Fatal(err)
	}
	before, _ = s.Snapshot(ctx, p.ParentID)
	if err := s.RecordWorktreeStop(ctx, record); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Snapshot(ctx, p.ParentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unchanged observation grew history")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || !reflect.DeepEqual(saved, record) {
		t.Fatal(saved, err)
	}
	record.Stopped = true
	if err := reopened.RecordWorktreeStop(ctx, record); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reopened.CancelWorktree(ctx, stop.OperationID, req.OperationID); err != nil {
			t.Fatal(err)
		}
	}
	op, err := reopened.WorktreeOperation(ctx, req.OperationID)
	if err != nil || op.State != "cancelled" || op.Failure == nil || op.Failure.Data.OperationID != stop.OperationID {
		t.Fatal(op, err)
	}
	record.Stopped = false
	if err := reopened.RecordWorktreeStop(ctx, record); err == nil {
		t.Fatal("settled observation reopened")
	}
	if _, err := reopened.TransitionWorktree(ctx, req, "cancelled", "ready", nil); err == nil {
		t.Fatal("cancelled checkout revived")
	}
	// Simulate the later tree-level completion; it still cannot replay this ID.
	if _, err := reopened.Append(ctx, p.ParentID, "", "tree_stop.completed", map[string]string{"operation_id": stop.OperationID}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Spawn(ctx, req, p, op.Workspace.Path, 0, 8, 4); err == nil {
		t.Fatal("cancelled workspace published a child after scope release")
	}
	if err := reopened.RecordWorktreeProcess(ctx, req, group); err != nil {
		t.Fatal("original record retry stopped being idempotent", err)
	}
	var count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM events WHERE kind='workspace.cancelled'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	plan, err := reopened.TreeStop(ctx, stop.OperationID)
	if err != nil || len(plan.Worktrees) != 1 {
		t.Fatal("scope history lost", plan, err)
	}
}

func TestWorktreeStopRejectsScopeChangesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	req, stop, p := worktreeStopFixture(t, s)
	group := process.Group{PID: 23456, Identity: "boot:10"}
	if err := s.RecordWorktreeProcess(ctx, req, group); err != nil {
		t.Fatal(err)
	}
	record, _ := s.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	wrong := record
	wrong.WorktreeID = "outside"
	if err := s.RecordWorktreeStop(ctx, wrong); err == nil {
		t.Fatal("out-of-scope workspace accepted")
	}
	if err := s.CancelWorktree(ctx, stop.OperationID, "outside"); err == nil {
		t.Fatal("out-of-scope workspace cancelled")
	}
	wrong = record
	wrong.Group = &process.Group{PID: 23457, Identity: group.Identity}
	if err := s.RecordWorktreeStop(ctx, wrong); err == nil {
		t.Fatal("stop redirected to another process")
	}
	for _, kind := range []string{"workspace.stop_observed", "workspace.cancelled"} {
		if kind == "workspace.cancelled" {
			record.Stopped = true
			if err := s.RecordWorktreeStop(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.db.Exec("CREATE TRIGGER reject_stop BEFORE INSERT ON events WHEN NEW.kind='" + kind + "' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
			t.Fatal(err)
		}
		before, _ := s.Snapshot(ctx, p.ParentID)
		var err error
		if kind == "workspace.cancelled" {
			err = s.CancelWorktree(ctx, stop.OperationID, req.OperationID)
		} else {
			record.Stopped = true
			err = s.RecordWorktreeStop(ctx, record)
		}
		if err == nil {
			t.Fatal("injected write failure ignored", kind)
		}
		after, _ := s.Snapshot(ctx, p.ParentID)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("partial stop transaction", kind)
		}
		op, _ := s.WorktreeOperation(ctx, req.OperationID)
		if op.State != "creating" {
			t.Fatal("cancellation without event", op)
		}
		if _, err := s.db.Exec("DROP TRIGGER reject_stop"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorktreeStopMissingIdentityAndMalformedEvidence(t *testing.T) {
	for _, state := range []string{"prepared", "creating", "unknown", "ready"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			req, p, plan := prepareWorkspaceFixture(t, s)
			if _, err := s.PrepareWorktree(ctx, req, p, plan, 0, 8, 4); err != nil {
				t.Fatal(err)
			}
			if state != "prepared" {
				if _, err := s.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
					t.Fatal(err)
				}
				if state != "creating" {
					if _, err := s.TransitionWorktree(ctx, req, "creating", state, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			stop := treeRequest(p.ParentID, "stop-worktree")
			if _, err := s.BeginTreeStop(ctx, stop, p.ParentID); err != nil {
				t.Fatal(err)
			}
			record, err := s.WorktreeStop(ctx, stop.OperationID, req.OperationID)
			if err != nil || record.Group != nil {
				t.Fatal(record, err)
			}
			// The real runtime also proves lock release before this transaction.
			if err := s.CancelWorktree(ctx, stop.OperationID, req.OperationID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordWorktreeProcess(ctx, req, process.Group{PID: 123, Identity: "boot:1"}); err == nil {
				t.Fatal("late unrecorded Start bypassed cancellation")
			}
		})
	}
	t.Run("corrupt original", func(t *testing.T) {
		ctx := context.Background()
		s, _ := newStore(t)
		req, stop, p := worktreeStopFixture(t, s)
		for _, group := range []process.Group{{PID: 0}, {PID: 1}, {PID: 1 << 31}, {PID: 123, Identity: "missing-start"}, {PID: 123, Witnesses: []process.GroupMember{{PID: 456, Identity: "boot:1"}}}} {
			if err := s.RecordWorktreeProcess(ctx, req, group); err == nil {
				t.Fatal("malformed original accepted", group)
			}
		}
		data, _ := json.Marshal(map[string]any{"operation_id": req.OperationID, "group": map[string]any{"pid": 123, "unexpected": true}})
		if _, err := s.Append(ctx, p.ParentID, "", "workspace.process", json.RawMessage(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WorktreeStop(ctx, stop.OperationID, req.OperationID); err == nil {
			t.Fatal("corrupt process treated as absent")
		}
		if err := s.CancelWorktree(ctx, stop.OperationID, req.OperationID); err == nil {
			t.Fatal("corrupt process permitted cancellation")
		}
	})
	t.Run("incomplete stop receipt", func(t *testing.T) {
		ctx := context.Background()
		s, _ := newStore(t)
		req, stop, p := worktreeStopFixture(t, s)
		if err := s.RecordWorktreeProcess(ctx, req, process.Group{PID: 123, Identity: "boot:1"}); err != nil {
			t.Fatal(err)
		}
		// A missing group must not inherit fields from the original identity.
		if _, err := s.Append(ctx, p.ParentID, "", "workspace.stop_observed", map[string]any{"operation_id": stop.OperationID, "worktree_id": req.OperationID, "stopped": true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WorktreeStop(ctx, stop.OperationID, req.OperationID); err == nil {
			t.Fatal("incomplete stop receipt inherited original group")
		}
		if err := s.CancelWorktree(ctx, stop.OperationID, req.OperationID); err == nil {
			t.Fatal("incomplete receipt completed cancellation")
		}
	})
}

func TestWorktreeProcessWriteFailureLeavesNoPartialIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	req, stop, p := worktreeStopFixture(t, s)
	if _, err := s.db.Exec("CREATE TRIGGER reject_process BEFORE INSERT ON events WHEN NEW.kind='workspace.process' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx, p.ParentID)
	if err := s.RecordWorktreeProcess(ctx, req, process.Group{PID: 123, Identity: "boot:1"}); err == nil {
		t.Fatal("failed process write reported success")
	}
	after, _ := s.Snapshot(ctx, p.ParentID)
	record, err := s.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || record.Group != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("partial identity receipt", record, err)
	}
}
