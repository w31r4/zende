package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func requestTreeStop(t *testing.T, c *Core, agent, id string) protocol.Request {
	t.Helper()
	req, _ := protocol.NewRequest("agent.stop", id, protocol.StopAgent{AgentID: agent, Tree: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Client lifetime must not own termination.
	got, err := c.Handle(ctx, req)
	if err != nil || got.(protocol.Accepted).OperationID != id {
		t.Fatal(got, err)
	}
	return req
}

func awaitTreeStop(t *testing.T, c *Core, id string) store.TreeStopRecord {
	t.Helper()
	var plan store.TreeStopRecord
	awaitWorkspace(t, "tree stop did not finish", func() bool {
		var err error
		plan, err = c.Store.TreeStop(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return plan.State == "stopped"
	})
	return plan
}

func treeChild(t *testing.T, c *Core, parent string) protocol.Accepted {
	t.Helper()
	p := protocol.SpawnAgent{ParentID: parent, Input: "child", Context: "none"}
	r, _ := protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), p)
	selection, err := c.Store.Selection(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Store.Spawn(context.Background(), r, p, selection.Choice.Cwd, selection.Choice.Revision, 8, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func cleanupTreeProcess(t *testing.T, m *process.Manager, s process.State) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = m.Signal(context.Background(), protocol.NewID("cleanup"), process.SignalParams{ID: s.ID, Signal: "KILL"})
		_, _ = m.Wait(context.Background(), s.ID, time.Second)
		if s.Terminal != nil {
			_ = exec.Command("tmux", "-S", s.Terminal.Socket, "kill-server").Run()
		}
	})
}

func TestTreeStopOwnsWholeTreeButPreservesUnrelatedWorkAndQueuedInput(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	ctx := context.Background()
	m, remote, _ := remoteRunner(t)
	entered := make(chan struct{}, 8)
	c := loopCore(t, adapterFunc(func(ctx context.Context, _ model.Request, _ func(model.Delta) error) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}))
	c.Config.Config.Limits.Agents = 8
	c.Runner = remote
	root := createWork(t, c, "root")
	child := treeChild(t, c, root.AgentID)
	grandchild := treeChild(t, c, child.AgentID)
	other := createWork(t, c, "other")
	runCore(t, c)
	for range 4 {
		awaitSignal(t, entered)
	}
	for i, a := range []protocol.Accepted{root, child, grandchild, other} {
		state, err := m.Start(ctx, process.Spec{ID: fmt.Sprintf("background-%d", i), AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Command: "read value", PTY: i == 1})
		if err != nil {
			t.Fatal(err)
		}
		cleanupTreeProcess(t, m, state)
	}
	queued := submitWork(t, c, child.AgentID, "later")
	mail := protocol.SendMessage{AgentID: grandchild.AgentID, Input: "retained", Delivery: "queue"}
	mailReq, _ := protocol.NewRequest("agent.send", "mail-before-stop", mail)
	acceptedMail, err := c.Store.Send(ctx, mailReq, mail, 8)
	if err != nil {
		t.Fatal(err)
	}
	req := requestTreeStop(t, c, root.AgentID, "whole-tree")
	plan := awaitTreeStop(t, c, req.OperationID)
	if len(plan.Members) != 3 {
		t.Fatal(plan)
	}
	for _, a := range []protocol.Accepted{root, child, grandchild} {
		snap := waitTask(t, c, a, "cancelled")
		if !snap.Agent.Paused {
			t.Fatal(snap)
		}
	}
	snap, err := c.Store.Snapshot(ctx, child.AgentID)
	if err != nil || snap.Tasks[1].ID != queued.TaskID || snap.Tasks[1].State != "accepted" {
		t.Fatal(snap, err)
	}
	if got, err := c.Store.Mail(ctx, acceptedMail.MessageID); err != nil || got.Input != "retained" {
		t.Fatal(got, err)
	}
	snap, err = c.Store.Snapshot(ctx, other.AgentID)
	if err != nil || snap.Agent.Paused || snap.Tasks[0].State != "running" {
		t.Fatal(snap, err)
	}
	if s, err := m.Inspect(ctx, "background-3"); err != nil || s.Status != "running" {
		t.Fatal(s, err)
	}
	proof, err := remote.InspectStop(ctx, req.OperationID)
	if err != nil || proof.State != "released" || len(proof.Processes) != 3 {
		t.Fatal(proof, err)
	}
	for _, p := range proof.Processes {
		if p.State != "stopped" {
			t.Fatal(proof)
		}
	}
	inspection, _ := protocol.NewRequest("operation.inspect", "", map[string]string{"operation_id": req.OperationID})
	got, err := c.Handle(ctx, inspection)
	if err != nil || !reflect.DeepEqual(got, plan) {
		t.Fatal(got, err)
	}
	controlAgent(t, c, "resume", root.AgentID, "resume-root")
	awaitSignal(t, entered)
	newProcess, err := m.Start(ctx, process.Spec{ID: "new-background", AgentID: root.AgentID, Cwd: c.Config.Config.Cwd, Command: "read value"})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTreeProcess(t, m, newProcess)
	if _, err := c.Handle(ctx, req); err != nil {
		t.Fatal(err)
	}
	if s, err := m.Inspect(ctx, newProcess.ID); err != nil || s.Status != "running" {
		t.Fatal("old stop affected new work", s, err)
	}
	if a, err := c.Store.Agent(ctx, root.AgentID); err != nil || a.Paused {
		t.Fatal(a, err)
	}
}

func TestTreeStopSettlesNeverStartedExecWithoutReplaying(t *testing.T) {
	ctx := context.Background()
	_, remote, _ := remoteRunner(t)
	script := fixture.New()
	c := loopCore(t, script, tool.ProcessExecutors(remote)...)
	c.Runner = remote
	a, appendEvent, _ := savedWork(t, c, script)
	call := model.ToolCall{ID: "call", Name: "exec_command", Arguments: json.RawMessage(`{"command":"touch marker"}`)}
	journalResponse(t, c, a, call)
	op := invocation{SchemaVersion: 2, ID: "late-dispatch", RequestID: "request-original", Call: call, Version: "2"}
	appendEvent("tool.started", op)
	appendEvent("tool.dispatching", op)
	req := requestTreeStop(t, c, a.AgentID, "cancel-undelivered")
	c = reopenCore(t, c) // Core vanished after acceptance, before runner freeze.
	c.Runner = remote
	c.Models = nil // Cancellation must need neither credentials nor a provider.
	runCore(t, c)
	awaitTreeStop(t, c, req.OperationID)
	waitTask(t, c, a, "cancelled")
	if len(script.Requests()) != 0 {
		t.Fatal("model called while cancelling")
	}
	if _, err := os.Stat(filepath.Join(c.Config.Config.Cwd, "marker")); !os.IsNotExist(err) {
		t.Fatal("command replayed", err)
	}
	if _, err := remote.Start(ctx, process.Spec{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Command: "touch marker"}); !isFailure(err, "process_cancelled") {
		t.Fatal("late command not fenced", err)
	}
	page, err := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, e := range page.Events {
		if e.Kind == "tool.verified" {
			verified++
		}
	}
	if verified != 1 {
		t.Fatal("missing or repeated cancellation evidence", verified)
	}
}

func TestTreeStopDoesNotClearUnknownPatch(t *testing.T) {
	ctx := context.Background()
	_, remote, _ := remoteRunner(t)
	script := fixture.New()
	c := loopCore(t, script, tool.DurablePatchExecutor(filepath.Join(t.TempDir(), "patches")))
	c.Runner = remote
	a, appendEvent, _ := savedWork(t, c, script)
	args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: marker\n+once\n*** End Patch"})
	call := model.ToolCall{ID: "patch", Name: "apply_patch", Arguments: args}
	journalResponse(t, c, a, call)
	_, descriptor, _ := c.Tools.Lookup(call.Name)
	op := invocation{SchemaVersion: 2, ID: "unknown-patch", RequestID: "request-original", Call: call, Version: descriptor.Version}
	appendEvent("tool.started", op)
	appendEvent("tool.dispatching", op)
	req := requestTreeStop(t, c, a.AgentID, "patch-stop")
	runCore(t, c)
	awaitWorkspace(t, "tree stop did not report held task", func() bool {
		p, err := c.Store.TreeStop(ctx, req.OperationID)
		return err == nil && p.Stage == "tasks" && p.Failure != nil
	})
	proof, err := remote.InspectStop(ctx, req.OperationID)
	if err != nil || proof.State != "stopped" {
		t.Fatal(proof, err)
	}
	resume, _ := protocol.NewRequest("agent.resume", "resume-unknown", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(ctx, resume); !isFailure(err, "tree_stopping") {
		t.Fatal(err)
	}
	if len(script.Requests()) != 0 {
		t.Fatal("unknown patch continued")
	}
	if _, err := os.Stat(filepath.Join(c.Config.Config.Cwd, "marker")); !os.IsNotExist(err) {
		t.Fatal("patch replayed", err)
	}
	p, err := c.Store.TreeStop(ctx, req.OperationID)
	if err != nil || p.State != "stopping" {
		t.Fatal(p, err)
	}
}

func TestTreeStopRecoversReleasedRunnerBeforeCompletionCommit(t *testing.T) {
	ctx := context.Background()
	_, remote, _ := remoteRunner(t)
	c := loopCore(t, nil)
	c.Runner = remote
	a := createWork(t, c, "")
	req := requestTreeStop(t, c, a.AgentID, "release-gap")
	db, err := sql.Open("sqlite", filepath.Join(c.Config.Config.Cwd, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER fail_complete BEFORE INSERT ON events WHEN NEW.kind='tree_stop.completed' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	stage, err := c.advanceTreeStop(ctx, req.OperationID)
	if stage != "release" || err == nil {
		t.Fatal(stage, err)
	}
	proof, err := remote.InspectStop(ctx, req.OperationID)
	if err != nil || proof.State != "released" {
		t.Fatal(proof, err)
	}
	resume, _ := protocol.NewRequest("agent.resume", "gap-resume", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(ctx, resume); !isFailure(err, "tree_stopping") {
		t.Fatal("released runner bypassed core fence", err)
	}
	if _, err := db.Exec("DROP TRIGGER fail_complete"); err != nil {
		t.Fatal(err)
	}
	c = reopenCore(t, c)
	c.Runner = remote
	c.Models = nil
	runCore(t, c)
	plan := awaitTreeStop(t, c, req.OperationID)
	if plan.OperationID != req.OperationID || len(plan.Members) != 1 {
		t.Fatal(plan)
	}
	after, err := remote.InspectStop(ctx, req.OperationID)
	if err != nil || !reflect.DeepEqual(proof, after) {
		t.Fatal("runner scope changed after core recovery", after, err)
	}
}

func TestTreeStopRejectsUnavailableRunnerBeforePausing(t *testing.T) {
	c := loopCore(t, nil)
	a := createWork(t, c, "queued")
	req, _ := protocol.NewRequest("agent.stop", "unavailable", protocol.StopAgent{AgentID: a.AgentID, Tree: true})
	if _, err := c.Handle(context.Background(), req); !isFailure(err, "unavailable") {
		t.Fatal(err)
	}
	snap, err := c.Store.Snapshot(context.Background(), a.AgentID)
	if err != nil || snap.Agent.Paused || snap.Tasks[0].State != "accepted" {
		t.Fatal(snap, err)
	}
	_, remote, _ := remoteRunner(t)
	c.Runner = remote
	if _, err := c.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	c.Runner = nil
	if _, err := c.Handle(context.Background(), req); err != nil {
		t.Fatal("accepted retry required current availability", err)
	}
}

func TestTreeStopDriverCancelsWorktreeWithoutModel(t *testing.T) {
	ctx := context.Background()
	_, remote, _ := remoteRunner(t)
	c := loopCore(t, nil)
	c.Runner = remote
	c.Models = nil
	req, p, gate, marker := gatedWorktree(t, c)
	done := make(chan error, 1)
	go func() { _, err := c.spawnAgent(ctx, req, p); done <- err }()
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("release"), 0600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("checkout handler remained live")
		}
	})
	awaitWorkspace(t, "checkout did not start", func() bool { _, err := os.Stat(marker); return err == nil })
	stop := requestTreeStop(t, c, p.ParentID, "tree-and-worktree")
	runCore(t, c)
	plan := awaitTreeStop(t, c, stop.OperationID)
	if len(plan.Worktrees) != 1 || plan.Worktrees[0] != req.OperationID {
		t.Fatal(plan)
	}
	if op, err := c.Store.WorktreeOperation(ctx, req.OperationID); err != nil || op.State != "cancelled" {
		t.Fatal(op, err)
	}
	if _, err := c.spawnAgent(ctx, req, p); !isFailure(err, "cancelled") {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "start\n" {
		t.Fatal("Git replayed", string(got), err)
	}
	if _, err := os.Stat(filepath.Join(p.WorktreePath, ".git")); err != nil {
		t.Fatal("partial checkout removed", err)
	}
}

type recoveringStopRunner struct {
	process.Remote
	available atomic.Bool
}

func (r *recoveringStopRunner) FreezeStop(ctx context.Context, id string, scope process.StopScope) (process.StopRecord, error) {
	if !r.available.Load() {
		return process.StopRecord{}, protocol.E("unavailable", "runner temporarily unavailable")
	}
	return r.Remote.FreezeStop(ctx, id, scope)
}

func TestTreeStopRetriesUnavailableRunnerWithoutUnpausing(t *testing.T) {
	_, remote, _ := remoteRunner(t)
	r := &recoveringStopRunner{Remote: remote}
	c := loopCore(t, nil)
	c.Runner = r
	c.Models = nil
	a := createWork(t, c, "")
	req := requestTreeStop(t, c, a.AgentID, "runner-retry")
	runCore(t, c)
	awaitWorkspace(t, "runner error was not recorded", func() bool {
		plan, err := c.Store.TreeStop(context.Background(), req.OperationID)
		return err == nil && plan.State == "stopping" && plan.Stage == "processes" && plan.Failure != nil && plan.Failure.Data.Kind == "unavailable"
	})
	resume, _ := protocol.NewRequest("agent.resume", "resume-while-unavailable", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(context.Background(), resume); !isFailure(err, "tree_stopping") {
		t.Fatal(err)
	}
	r.available.Store(true)
	plan := awaitTreeStop(t, c, req.OperationID)
	if plan.Failure != nil {
		t.Fatal("completed stop retained old error", plan)
	}
}

func TestTreeStopWaitsForUncancellableToolSettlement(t *testing.T) {
	_, remote, _ := remoteRunner(t)
	entered, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	c := loopCore(t, fixture.New(fixture.Step{Events: fixture.Call("slow", "uncancellable", `{}`)}), testTool{"uncancellable", func(context.Context, tool.Call) (tool.Result, error) {
		close(entered)
		<-released
		return tool.Result{Content: []model.Block{{Type: "text", Text: "original effect settled"}}}, nil
	}})
	c.Runner = remote
	a := createWork(t, c, "work")
	runCore(t, c)
	t.Cleanup(release) // Before the scheduler cleanup if an assertion fails.
	awaitSignal(t, entered)
	req := requestTreeStop(t, c, a.AgentID, "wait-original-worker")
	awaitWorkspace(t, "tree stop did not wait for original worker", func() bool {
		plan, err := c.Store.TreeStop(context.Background(), req.OperationID)
		return err == nil && plan.Stage == "tasks" && plan.State == "stopping"
	})
	if snap, err := c.Store.Snapshot(context.Background(), a.AgentID); err != nil || snap.Tasks[0].State != "cancelling" {
		t.Fatal(snap, err)
	}
	release()
	awaitTreeStop(t, c, req.OperationID)
	waitTask(t, c, a, "cancelled")
}
