package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

func requireStopError(t *testing.T, err error, kind string) {
	t.Helper()
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func finishStop(t *testing.T, m StopController, op string) StopRecord {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		r, err := m.AdvanceStop(context.Background(), op)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "stopped" {
			return r
		}
	}
	r, _ := m.InspectStop(context.Background(), op)
	boot, _ := kernelBoot()
	for _, p := range r.Processes {
		if p.PID > 1 {
			members, err := kernelScope(p, boot)
			t.Logf("remaining %s: %+v (%v)", p.ID, members, err)
		}
	}
	t.Fatalf("scope did not stop: %+v", r)
	return r
}

func TestScopeStopPipeFreezeTerminateAndLateDispatch(t *testing.T) {
	m := manager(t, 8192)
	ctx := context.Background()
	cwd := t.TempDir()
	spec := Spec{ID: "owned", AgentID: "parent", TaskID: "task", Cwd: cwd, Command: "printf before; cat"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, spec.ID)
	waitOutput(t, m, spec.ID, "stdout", "before")
	input := InputParams{ID: spec.ID, Data: []byte("hello\n")}
	if _, err := m.Input(ctx, "input-once", input); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, spec.ID, "stdout", "hello")
	other := Spec{ID: "outside", AgentID: "outside", TaskID: "other-task", Cwd: cwd, Command: "sleep 60"}
	if _, err := m.Start(ctx, other); err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, other.ID)
	late := StopDispatch{ID: "late", AgentID: "child", TaskID: "child-task"}
	scope := StopScope{Agents: []string{"parent", "child"}, Pending: []StopDispatch{late}}
	r, err := m.FreezeStop(ctx, "stop-tree", scope)
	if err != nil || r.State != "frozen" || len(r.Processes) != 2 {
		t.Fatal(r, err)
	}
	if state, err := m.Inspect(ctx, spec.ID); err != nil || state.Status != "running" {
		t.Fatal("freeze itself killed work", state, err)
	}
	if again, err := m.Start(ctx, spec); err != nil || again.PID != started.PID {
		t.Fatal("existing receipt lost", again, err)
	}
	if _, err := m.Input(ctx, "input-once", input); err != nil {
		t.Fatal("completed input retry lost", err)
	}
	_, err = m.Input(ctx, "input-twice", input)
	requireStopError(t, err, "tree_stopping")
	_, err = m.Start(ctx, Spec{ID: "new", AgentID: "parent", Cwd: cwd, Command: "touch forbidden"})
	requireStopError(t, err, "tree_stopping")
	_, err = m.ReleaseStop(ctx, "stop-tree")
	requireStopError(t, err, "tree_stopping")
	_, err = m.FreezeStop(ctx, "overlap", StopScope{Agents: []string{"child"}})
	requireStopError(t, err, "tree_stopping")
	// Order is not part of scope identity; mutation of a returned slice cannot
	// change the stored receipt or its admission gate.
	r.Scope.Agents[0] = "changed"
	r.Processes[0].ID = "changed"
	again, err := m.FreezeStop(ctx, "stop-tree", StopScope{Agents: []string{"child", "parent"}, Pending: []StopDispatch{late}})
	if err != nil || again.Processes[0].ID == "changed" || again.Scope.Agents[0] != "child" {
		t.Fatal(again, err)
	}
	_, err = m.FreezeStop(ctx, "stop-tree", StopScope{Agents: []string{"parent"}})
	requireStopError(t, err, "operation_conflict")
	finishStop(t, m, "stop-tree")
	state, err := m.Wait(ctx, spec.ID, 3*time.Second)
	if err != nil || state.Status != "exited" || state.ExitSignal != "killed" {
		t.Fatal(state, err)
	}
	out, err := m.Read(ctx, spec.ID, "stdout", 0, 8192)
	if err != nil || string(out.Data) != "beforehello\n" {
		t.Fatal(out, err)
	}
	if state, err := m.Inspect(ctx, other.ID); err != nil || state.Status != "running" {
		t.Fatal("unrelated process affected", state, err)
	}
	// Kernel completion does not implicitly reopen the Agent's input/start gate.
	_, err = m.Start(ctx, Spec{ID: "new", AgentID: "parent", Cwd: cwd, Command: "true"})
	requireStopError(t, err, "tree_stopping")
	if r, err = m.ReleaseStop(ctx, "stop-tree"); err != nil || r.State != "released" {
		t.Fatal(r, err)
	}
	_, err = m.Start(ctx, Spec{ID: late.ID, AgentID: late.AgentID, TaskID: late.TaskID, Cwd: cwd, Command: "touch forbidden"})
	requireStopError(t, err, "process_cancelled")
	// A changed owner cannot recycle the cancelled process identity either.
	_, err = m.Start(ctx, Spec{ID: late.ID, AgentID: "outside", Cwd: cwd, Command: "touch forbidden"})
	requireStopError(t, err, "process_cancelled")
	newSpec := Spec{ID: "new-after", AgentID: "parent", Cwd: cwd, Command: "sleep 60"}
	if _, err := m.Start(ctx, newSpec); err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, newSpec.ID)
	if r, err := m.AdvanceStop(ctx, "stop-tree"); err != nil || r.State != "released" {
		t.Fatal(r, err)
	}
	if state, _ := m.Inspect(ctx, newSpec.ID); state.Status != "running" {
		t.Fatal("old stop captured new work", state)
	}
	if _, err := os.Stat(filepath.Join(cwd, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("late command executed", err)
	}
}

func TestScopeStopPTYAndRunnerReopen(t *testing.T) {
	m := tmuxManager(t, 8192)
	ctx := context.Background()
	spec := Spec{ID: "terminal-stop", AgentID: "agent", TaskID: "task", PTY: true, Cwd: t.TempDir(), Command: "set -m; sleep 60 & printf ready; wait"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, spec.ID, "pty", "ready")
	scope := StopScope{Agents: []string{"agent"}, Pending: []StopDispatch{{ID: "late-pty", AgentID: "agent", TaskID: "task"}}}
	if _, err := m.FreezeStop(ctx, "pty-stop", scope); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 8192)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reopened.Start(ctx, Spec{ID: "new-pty", AgentID: "agent", PTY: true, Cwd: spec.Cwd, Command: "true"})
	requireStopError(t, err, "tree_stopping")
	r := finishStop(t, reopened, "pty-stop")
	if len(r.Processes) != 2 {
		t.Fatal(r)
	}
	state, err := reopened.Wait(ctx, spec.ID, 5*time.Second)
	if err != nil || state.Status != "exited" || state.ExitSignal != "9" && state.ExitSignal != "killed" {
		t.Fatal(state, err)
	}
	if pane, err := inspectPane(m.tmux, *started.Terminal); err != nil || !pane.Dead {
		t.Fatal("owned history pane was deleted or kept running", pane, err)
	}
	if out, err := reopened.Read(ctx, spec.ID, "pty", 0, 8192); err != nil || !strings.Contains(string(out.Data), "ready") {
		t.Fatal(out, err)
	}
	if _, err := reopened.ReleaseStop(ctx, "pty-stop"); err != nil {
		t.Fatal(err)
	}
	third, err := NewManager(m.root, m.shell, 4, 8192)
	if err != nil {
		t.Fatal(err)
	}
	_, err = third.Start(ctx, Spec{ID: "late-pty", AgentID: "agent", TaskID: "task", PTY: true, Cwd: spec.Cwd, Command: "true"})
	requireStopError(t, err, "process_cancelled")
	if got, err := third.FreezeStop(ctx, "pty-stop", scope); err != nil || got.State != "released" {
		t.Fatal(got, err)
	}
}

func TestScopeStopConcurrentStartIsCapturedOrRejected(t *testing.T) {
	for i := range 25 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			m := manager(t, 4096)
			ctx := context.Background()
			spec := Spec{ID: "racing", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "sleep 60"}
			stopTestProcess(t, m, spec.ID)
			ready := make(chan struct{})
			var wg sync.WaitGroup
			var startErr, freezeErr error
			var record StopRecord
			wg.Add(2)
			go func() { defer wg.Done(); <-ready; _, startErr = m.Start(ctx, spec) }()
			go func() {
				defer wg.Done()
				<-ready
				record, freezeErr = m.FreezeStop(ctx, "race-stop", StopScope{Agents: []string{"agent"}})
			}()
			close(ready)
			wg.Wait()
			if freezeErr != nil {
				t.Fatal(freezeErr)
			}
			if startErr == nil {
				if len(record.Processes) != 1 || record.Processes[0].ID != spec.ID {
					t.Fatal("accepted process escaped captured scope", record)
				}
			} else {
				requireStopError(t, startErr, "tree_stopping")
				if len(record.Processes) != 0 {
					t.Fatal(record)
				}
			}
			finishStop(t, m, "race-stop")
		})
	}
}

func TestScopeStopUnknownIdentityNeverSignalsOrReleases(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "uncertain", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "sleep 60"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-started.PID, syscall.SIGKILL); _, _ = m.Wait(ctx, spec.ID, 3*time.Second) })
	e, _ := m.get(spec.ID)
	e.mu.Lock()
	e.state.Identity = strings.Split(started.Identity, ":")[0] + ":0"
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.mu.Unlock()
	if _, err := m.FreezeStop(ctx, "unknown-stop", StopScope{Agents: []string{"agent"}}); err != nil {
		t.Fatal(err)
	}
	r, err := m.AdvanceStop(ctx, "unknown-stop")
	if err != nil || r.State != "frozen" || r.Processes[0].State != "unknown" || r.Processes[0].Failure == "" {
		t.Fatal(r, err)
	}
	if identity(started.PID) != started.Identity {
		t.Fatal("unverified process was signalled")
	}
	_, err = m.ReleaseStop(ctx, "unknown-stop")
	requireStopError(t, err, "tree_stopping")
	// External verified termination may resolve absence, without inventing an
	// exit code or replaying the original command.
	if err := syscall.Kill(-started.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(ctx, spec.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	finishStop(t, m, "unknown-stop")
}

func TestScopeStopSavedWitnessSurvivesLeaderExit(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	// The shell exits after release; its child keeps the original process
	// group alive. Saved witness evidence must stop the child after reopen.
	cwd := t.TempDir()
	spec := Spec{ID: "descendant", AgentID: "agent", TaskID: "task", Cwd: cwd, Command: "sleep 60 & printf ready; while [ ! -f release ]; do sleep 0.01; done"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-started.PID, syscall.SIGKILL); _, _ = m.Wait(ctx, spec.ID, 3*time.Second) })
	waitOutput(t, m, spec.ID, "stdout", "ready")
	r, err := m.FreezeStop(ctx, "descendant-stop", StopScope{Agents: []string{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	witnesses, ended, err := stopEvidence(r.Processes[0])
	if err != nil || ended || len(witnesses) < 2 {
		t.Fatal(witnesses, ended, err)
	}
	r.Processes[0].Witnesses = witnesses
	if err := m.saveStop(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := m.Wait(ctx, spec.ID, 4*time.Second); err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	finishStop(t, reopened, "descendant-stop")
	// A shell's saved successful exit alone would not have proved this.
	if _, ended, err := stopEvidence(r.Processes[0]); err != nil || !ended {
		t.Fatal("descendant survived", ended, err)
	}
}

func TestScopeStopInvalidMetadataHoldsAdmission(t *testing.T) {
	for _, damage := range []string{"json", "version", "fingerprint", "pending-missing", "false-completion", "symlink", "directory"} {
		t.Run(damage, func(t *testing.T) {
			m := manager(t, 4096)
			ctx := context.Background()
			r, err := m.FreezeStop(ctx, "damaged", StopScope{Agents: []string{"agent"}, Pending: []StopDispatch{{ID: "late", AgentID: "agent", TaskID: "task"}}})
			if err != nil {
				t.Fatal(err)
			}
			path := m.stopPath(r.OperationID)
			switch damage {
			case "json":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "version":
				r.Version++
				err = saveJSON(path, r)
			case "fingerprint":
				r.Fingerprint = "wrong"
				err = saveJSON(path, r)
			case "pending-missing":
				r.Processes = nil
				err = saveJSON(path, r)
			case "false-completion":
				r.State, r.Processes[0].State = "released", "unknown"
				err = saveJSON(path, r)
			case "symlink", "directory":
				if err = os.Remove(path); err == nil {
					if damage == "symlink" {
						err = os.Symlink(filepath.Join(m.root, "absent"), path)
					} else {
						err = os.Mkdir(path, 0700)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := NewManager(m.root, m.shell, 4, 4096)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reopened.Start(ctx, Spec{ID: "new", AgentID: "outside", Cwd: t.TempDir(), Command: "true"})
			requireStopError(t, err, "invalid_state")
			_, err = reopened.InspectStop(ctx, r.OperationID)
			requireStopError(t, err, "invalid_state")
			_, err = reopened.ReleaseStop(ctx, r.OperationID)
			requireStopError(t, err, "invalid_state")
			if page, err := reopened.List(ctx, ListParams{}); err != nil || len(page.Processes) != 0 {
				t.Fatal("read-only recovery was blocked", page, err)
			}
		})
	}
}

func TestScopeStopFailedSaveNeverAllowsNewEffects(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	if err := os.MkdirAll(m.stopPath("write-failure"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := m.FreezeStop(ctx, "write-failure", StopScope{Agents: []string{"agent"}})
	requireStopError(t, err, "transport_unknown")
	_, err = m.Start(ctx, Spec{ID: "new", AgentID: "agent", Cwd: t.TempDir(), Command: "true"})
	requireStopError(t, err, "invalid_state")
}

func TestScopeStopValidatesWholeScopeBeforeFreezing(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	for _, scope := range []StopScope{
		{}, {Agents: []string{"a", "a"}}, {Agents: []string{"../invalid"}},
		{Agents: []string{"a"}, Pending: []StopDispatch{{ID: "p", AgentID: "outside", TaskID: "task"}}},
		{Agents: []string{"a"}, Pending: []StopDispatch{{ID: "p", AgentID: "a"}}},
		{Agents: []string{"a"}, Pending: []StopDispatch{{ID: "p", AgentID: "a", TaskID: "task"}, {ID: "p", AgentID: "a", TaskID: "task"}}},
		{Agents: make([]string, maxStopItems+1)},
	} {
		_, err := m.FreezeStop(ctx, "invalid", scope)
		requireStopError(t, err, "invalid_params")
	}
	if len(m.stops) != 0 {
		t.Fatal("invalid scope had effects")
	}
	_, err := m.FreezeStop(ctx, "../invalid", StopScope{Agents: []string{"a"}})
	requireStopError(t, err, "invalid_params")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.FreezeStop(ctx, "cancelled", StopScope{Agents: []string{"a"}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestScopeStopRemoteContract(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	socket := filepath.Join(t.TempDir(), "runner.sock")
	server, err := protocol.Listen(socket, Service{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	defer func() { cancel(); <-done }()
	r := Remote{Client: protocol.Client{Socket: socket}}
	scope := StopScope{Agents: []string{"agent"}, Pending: []StopDispatch{{ID: "late", AgentID: "agent", TaskID: "task"}}}
	frozen, err := r.FreezeStop(ctx, "rpc-stop", scope)
	if err != nil || frozen.State != "frozen" {
		t.Fatal(frozen, err)
	}
	if got, err := r.InspectStop(ctx, frozen.OperationID); err != nil || !reflect.DeepEqual(got, frozen) {
		t.Fatal(got, err)
	}
	finishStop(t, r, frozen.OperationID)
	if released, err := r.ReleaseStop(ctx, frozen.OperationID); err != nil || released.State != "released" {
		t.Fatal(released, err)
	}
	_, err = r.Start(ctx, Spec{ID: "late", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "true"})
	requireStopError(t, err, "process_cancelled")
	for _, method := range []string{"process.stop.advance", "process.stop.release"} {
		req, _ := protocol.NewRequest(method, "wrong-stop", StopTarget{OperationID: frozen.OperationID})
		_, err := (Service{Manager: m}).Handle(ctx, req)
		requireStopError(t, err, "invalid_params")
	}
}

func TestScopeStopKeepsProcessIdentityWhenMetadataDisappears(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "once", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "printf x >> marker"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(ctx, spec.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FreezeStop(ctx, "stop-once", StopScope{Agents: []string{"agent"}}); err != nil {
		t.Fatal(err)
	}
	finishStop(t, m, "stop-once")
	if _, err := m.ReleaseStop(ctx, "stop-once"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(m.root, spec.ID), filepath.Join(m.root, ".saved-once")); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reopened.Start(ctx, spec)
	requireStopError(t, err, "operation_unknown")
	data, err := os.ReadFile(filepath.Join(spec.Cwd, "marker"))
	if err != nil || string(data) != "x" {
		t.Fatal("missing metadata replayed the stopped command", string(data), err)
	}
}

func TestScopeStopUnknownOwnershipAndCapacityAreAtomic(t *testing.T) {
	t.Run("unknown-owner", func(t *testing.T) {
		m := manager(t, 4096)
		m.entries["damaged"] = &entry{quarantined: true, state: State{Spec: Spec{ID: "damaged"}, Status: "unknown"}}
		_, err := m.FreezeStop(context.Background(), "stop", StopScope{Agents: []string{"agent"}})
		requireStopError(t, err, "operation_unknown")
		if len(m.stops) != 0 {
			t.Fatal("unknown ownership was silently omitted")
		}
	})
	t.Run("too-many-processes", func(t *testing.T) {
		m := manager(t, 4096)
		for i := range maxStopItems + 1 {
			id := fmt.Sprintf("process-%d", i)
			m.entries[id] = &entry{state: State{Spec: Spec{ID: id, AgentID: "agent"}, Status: "failed"}}
		}
		_, err := m.FreezeStop(context.Background(), "stop", StopScope{Agents: []string{"agent"}})
		requireStopError(t, err, "limit_exceeded")
		if len(m.stops) != 0 {
			t.Fatal("capacity rejection captured a partial scope")
		}
	})
	t.Run("other-owner", func(t *testing.T) {
		m := manager(t, 4096)
		m.entries["p"] = &entry{state: State{Spec: Spec{ID: "p", AgentID: "outside", TaskID: "task"}, Status: "failed"}}
		_, err := m.FreezeStop(context.Background(), "stop", StopScope{Agents: []string{"agent"}, Pending: []StopDispatch{{ID: "p", AgentID: "agent", TaskID: "task"}}})
		requireStopError(t, err, "operation_conflict")
		if len(m.stops) != 0 {
			t.Fatal("conflicting owner was captured")
		}
	})
}

func TestScopeStopFreezeWaitsForAdmittedInput(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "stdin-race", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "cat"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, spec.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	inputDone := make(chan error, 1)
	go func() {
		_, err := m.control(ctx, spec.ID, "held-input", "process.stdin", InputParams{ID: spec.ID, Data: []byte("once")}, func(e *entry) (ControlResult, error) {
			close(entered)
			<-release
			n, err := e.stdin.Write([]byte("once"))
			return ControlResult{Written: n}, err
		})
		inputDone <- err
	}()
	<-entered
	stopDone := make(chan error, 1)
	go func() {
		_, err := m.FreezeStop(ctx, "input-stop", StopScope{Agents: []string{"agent"}})
		stopDone <- err
	}()
	select {
	case err := <-stopDone:
		close(release)
		<-inputDone
		t.Fatal("freeze passed an in-flight input dispatch", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-inputDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, spec.ID, "stdout", "once")
	finishStop(t, m, "input-stop")
	if _, err := m.Wait(ctx, spec.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	out, err := m.Read(ctx, spec.ID, "stdout", 0, 4096)
	if err != nil || string(out.Data) != "once" {
		t.Fatal(out, err)
	}
}

func TestScopeStopLateForkUsesOriginalGroupHandle(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "late-fork", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "printf ready; read line; sleep 60 & printf forked; wait"}
	state, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-state.PID, syscall.SIGKILL); _, _ = m.Wait(ctx, spec.ID, 3*time.Second) })
	waitOutput(t, m, spec.ID, "stdout", "ready")
	// Probe a harmless signal on this owned group; old kernels take the
	// separately tested per-member fallback, which cannot promise late forks.
	fd, err := unix.PidfdOpen(state.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = unix.PidfdSendSignal(fd, 0, nil, unix.PIDFD_SIGNAL_PROCESS_GROUP)
	unix.Close(fd)
	if errors.Is(err, unix.EINVAL) {
		t.Skip("kernel lacks pidfd process-group signalling")
	}
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.FreezeStop(ctx, "fork-stop", StopScope{Agents: []string{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Processes[0]
	p.Witnesses, _, err = stopEvidence(p)
	if err != nil || len(p.Witnesses) != 1 {
		t.Fatal(p, err)
	}
	r.Processes[0] = p
	if err := m.saveStop(r); err != nil {
		t.Fatal(err)
	}
	// Deliberately create a child after the saved observation, reproducing the
	// scheduling window without probabilistic sleeps or production test hooks.
	e, _ := m.get(spec.ID)
	if _, err := e.stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, spec.ID, "stdout", "forked")
	if err := killStopMembers(p); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(ctx, spec.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ended, err := stopEvidence(p); err != nil || !ended {
		t.Fatal("late child escaped the group handle", ended, err)
	}
	finishStop(t, m, "fork-stop")
}

func TestScopeStopGroupFallbackUsesSameHandleOnly(t *testing.T) {
	for _, failure := range []error{nil, unix.EINVAL, unix.ESRCH, unix.EPERM} {
		var flags []int
		err := killHandle(123, unix.PIDFD_SIGNAL_PROCESS_GROUP, func(fd int, sig unix.Signal, info *unix.Siginfo, f int) error {
			if fd != 123 || sig != unix.SIGKILL || info != nil {
				t.Fatal("handle or signal changed")
			}
			flags = append(flags, f)
			if len(flags) == 1 {
				return failure
			}
			return nil
		})
		want := []int{unix.PIDFD_SIGNAL_PROCESS_GROUP}
		if errors.Is(failure, unix.EINVAL) {
			want = append(want, 0)
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(flags, want) {
			t.Fatal(flags, want)
		}
	}
}

func TestScopeStopProvedExitReleasesCapacityWithoutInventingResult(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "old-pipe", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Command: "printf ready; sleep 60"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, spec.ID)
	waitOutput(t, m, spec.ID, "stdout", "ready")
	if _, err := m.FreezeStop(ctx, "capacity-stop", StopScope{Agents: []string{"agent"}}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, m.shell, 1, 4096)
	if err != nil {
		t.Fatal(err)
	}
	finishStop(t, reopened, "capacity-stop")
	if _, err := reopened.ReleaseStop(ctx, "capacity-stop"); err != nil {
		t.Fatal(err)
	}
	state, err := reopened.Inspect(ctx, spec.ID)
	if err != nil || state.Status != "unknown" || state.ExitCode != nil {
		t.Fatal("stop proof invented a pipe exit result", state, err)
	}
	if _, err := reopened.Start(ctx, Spec{ID: "new-pipe", AgentID: "agent", Cwd: spec.Cwd, Command: "true"}); err != nil {
		t.Fatal("verified stopped work still consumes process capacity", err)
	}
	if _, err := reopened.Wait(ctx, "new-pipe", 3*time.Second); err != nil {
		t.Fatal(err)
	}
}
