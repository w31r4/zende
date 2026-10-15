package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func stopRunner(t *testing.T) *process.Manager {
	t.Helper()
	m, err := process.NewManager(t.TempDir(), "/bin/sh", 8, 65536)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTreeRunnerScopeIsImmutableAndCapturesDispatchedIdentities(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, root := controlWork(t, s)
	if _, err := s.ClaimNext(ctx, root.AgentID); err != nil {
		t.Fatal(err)
	}
	child := resultChild(t, s, root.AgentID)
	appendCall := func(a protocol.Accepted, kind, id, name string, version int) {
		t.Helper()
		if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, kind, map[string]any{
			"operation_id": id, "schema_version": version, "call": model.ToolCall{ID: id, Name: name},
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendCall(root, "tool.dispatching", "historical", "exec_command", 2)
	appendCall(root, "tool.dispatching", "historical", "exec_command", 2)
	appendCall(root, "tool.started", "prepared-only", "exec_command", 2)
	appendCall(root, "tool.dispatching", "patch", "apply_patch", 2)
	appendCall(child, "tool.started", "legacy", "exec_command", 1)
	appendCall(child, "tool.dispatching", "child-exec", "exec_command", 2)
	if err := s.FinishTask(ctx, root.AgentID, root.TaskID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	req := treeRequest(root.AgentID, "scope")
	if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
		t.Fatal(err)
	}
	got, err := s.TreeStopRunnerScope(ctx, req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	want := process.StopScope{Agents: []string{root.AgentID, child.AgentID}, Pending: []process.StopDispatch{
		{ID: "child-exec", AgentID: child.AgentID, TaskID: child.TaskID},
		{ID: "historical", AgentID: root.AgentID, TaskID: root.TaskID},
		{ID: "legacy", AgentID: child.AgentID, TaskID: child.TaskID},
	}}
	slices.Sort(want.Agents)
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got, want)
	}
	before, _ := s.Snapshot(ctx, root.AgentID)
	again, err := s.TreeStopRunnerScope(ctx, req.OperationID)
	after, _ := s.Snapshot(ctx, root.AgentID)
	if err != nil || !reflect.DeepEqual(got, again) || !reflect.DeepEqual(before, after) {
		t.Fatal(again, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err = s.TreeStopRunnerScope(ctx, req.OperationID)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatal(again, err)
	}
}

func TestTreeRunnerScopeDoesNotSilentlyTruncateDuplicateEvents(t *testing.T) {
	for _, distinct := range []bool{false, true} {
		t.Run(fmt.Sprint(distinct), func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, root := controlWork(t, s)
			if _, err := s.ClaimNext(ctx, root.AgentID); err != nil {
				t.Fatal(err)
			}
			err := s.Transaction(ctx, func(tx *sql.Tx) error {
				for i := range 10002 {
					id := "repeated"
					if distinct || i == 10001 {
						id = fmt.Sprintf("dispatch-%d", i)
					}
					if _, err := appendEvent(ctx, tx, root.AgentID, root.TaskID, "tool.dispatching", map[string]any{"operation_id": id, "call": model.ToolCall{Name: "exec_command"}}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			req := treeRequest(root.AgentID, "bounded")
			if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
				t.Fatal(err)
			}
			scope, err := s.TreeStopRunnerScope(ctx, req.OperationID)
			if !distinct {
				if err != nil || len(scope.Pending) != 2 || scope.Pending[0].ID != "dispatch-10001" {
					t.Fatal(scope, err)
				}
			} else {
				if err == nil {
					t.Fatal("partial oversized scope saved")
				}
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM events WHERE kind='tree_stop.runner_scope'").Scan(&count); err != nil || count != 0 {
					t.Fatal(count, err)
				}
			}
		})
	}
}

func TestTreeStopCompletionRequiresReleasedMatchingReceiptAndSettledTasks(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, root := controlWork(t, s)
	if _, err := s.ClaimNext(ctx, root.AgentID); err != nil {
		t.Fatal(err)
	}
	child := resultChild(t, s, root.AgentID)
	req := treeRequest(root.AgentID, "complete")
	if _, err := s.BeginTreeStop(ctx, req, root.AgentID); err != nil {
		t.Fatal(err)
	}
	scope, err := s.TreeStopRunnerScope(ctx, req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	m := stopRunner(t)
	if _, err := m.FreezeStop(ctx, req.OperationID, scope); err != nil {
		t.Fatal(err)
	}
	stopped, err := m.AdvanceStop(ctx, req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTreeStop(ctx, req.OperationID, stopped); err == nil {
		t.Fatal("unreleased runner accepted")
	}
	released, err := m.ReleaseStop(ctx, req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTreeStop(ctx, req.OperationID, released); err == nil {
		t.Fatal("live tasks accepted")
	}
	for _, a := range []protocol.Accepted{root, child} {
		if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "late", nil); err != nil {
			t.Fatal(err)
		}
	}
	wrong := released
	wrong.OperationID = "other-stop"
	if err := s.CompleteTreeStop(ctx, req.OperationID, wrong); err == nil {
		t.Fatal("unrelated proof accepted")
	}
	if _, err := s.db.Exec("CREATE TRIGGER fail_complete BEFORE INSERT ON events WHEN NEW.kind='tree_stop.completed' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTreeStop(ctx, req.OperationID, released); err == nil {
		t.Fatal("failed completion reported success")
	}
	for _, a := range []protocol.Accepted{root, child} {
		_, err := s.Resume(ctx, resumeRequest(a.AgentID, protocol.NewID("resume")), protocol.Target{AgentID: a.AgentID}, 8)
		requireTreeFence(t, err, req.OperationID)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_complete"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTreeStop(ctx, req.OperationID, released); err != nil {
		t.Fatal(err)
	}
	plan, err := s.TreeStop(ctx, req.OperationID)
	if err != nil || plan.State != "stopped" || plan.Stage != "completed" {
		t.Fatal(plan, err)
	}
	if id, err := s.ActiveTreeStop(ctx, child.AgentID); err != nil || id != "" {
		t.Fatal(id, err)
	}
	for _, a := range []protocol.Accepted{root, child} {
		snap, err := s.Snapshot(ctx, a.AgentID)
		if err != nil || !snap.Agent.Paused {
			t.Fatal(snap, err)
		}
	}
	before, _ := s.Snapshot(ctx, root.AgentID)
	if err := s.CompleteTreeStop(ctx, req.OperationID, released); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Snapshot(ctx, root.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("completion retry appended")
	}
}

func TestPendingTreeStopsPaginationAndProgressDedup(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	for i := range 35 {
		a := mailAgent(t, s)
		req := treeRequest(a.AgentID, fmt.Sprintf("stop-%d", i))
		if _, err := s.BeginTreeStop(ctx, req, a.AgentID); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.PendingTreeStops(ctx, 0)
	if err != nil || len(first) != 32 {
		t.Fatal(first, err)
	}
	last, err := s.PendingTreeStops(ctx, first[31].Cursor)
	if err != nil || len(last) != 3 || last[0].ID != "stop-32" {
		t.Fatal(last, err)
	}
	failure := protocol.E("unavailable", "runner unavailable")
	for range 2 {
		if err := s.RecordTreeStopProgress(ctx, "stop-0", "processes", failure); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM events WHERE kind='tree_stop.progress'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	plan, err := s.TreeStop(ctx, "stop-0")
	if err != nil || plan.Stage != "processes" || !reflect.DeepEqual(plan.Failure, failure) {
		t.Fatal(plan, err)
	}
	scope, err := s.TreeStopRunnerScope(ctx, "stop-0")
	if err != nil {
		t.Fatal(err)
	}
	m := stopRunner(t)
	if _, err := m.FreezeStop(ctx, "stop-0", scope); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AdvanceStop(ctx, "stop-0"); err != nil {
		t.Fatal(err)
	}
	receipt, err := m.ReleaseStop(ctx, "stop-0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTreeStop(ctx, "stop-0", receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTreeStopProgress(ctx, "stop-0", "tasks", failure); err != nil {
		t.Fatal(err)
	}
	plan, err = s.TreeStop(ctx, "stop-0")
	if err != nil || plan.Stage != "completed" || plan.Failure != nil {
		t.Fatal(plan, err)
	}
	first, err = s.PendingTreeStops(ctx, 0)
	if err != nil || len(first) != 32 || first[0].ID != "stop-1" {
		t.Fatal(first, err)
	}
}
