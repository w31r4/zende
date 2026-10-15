package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

func controlAgent(t *testing.T, c *Core, method, id, op string) protocol.Accepted {
	t.Helper()
	req, _ := protocol.NewRequest("agent."+method, op, protocol.Target{AgentID: id})
	value, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.Accepted)
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("test signal timed out")
	}
}

func TestStopModelStreamPersistsPauseAndResumeUsesNewTask(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		if requests.Add(1) == 1 {
			for _, d := range []model.Delta{{Kind: model.MessageStart, MessageID: "partial", Category: "commentary"}, {Kind: model.TextDelta, MessageID: "partial", Text: "partial note"}} {
				if err := emit(d); err != nil {
					return err
				}
			}
			close(entered)
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		}
		for _, d := range fixture.Answer("final", "new task complete") {
			if err := emit(d); err != nil {
				return err
			}
		}
		return nil
	})
	c := loopCore(t, adapter)
	a := createWork(t, c, "original input")
	runCore(t, c)
	awaitSignal(t, entered)
	stop := controlAgent(t, c, "stop", a.AgentID, "stop-stream")
	if stop.TaskID != a.TaskID {
		t.Fatal(stop)
	}
	awaitSignal(t, cancelled)
	snap := waitTask(t, c, a, "cancelled")
	if snap.Agent.State != "paused" || !snap.Agent.Paused {
		t.Fatal(snap)
	}
	res := controlAgent(t, c, "resume", a.AgentID, "resume-stream")
	if res.TaskID == a.TaskID {
		t.Fatal("old task revived")
	}
	snap = waitTask(t, c, res, "completed")
	if snap.Tasks[0].State != "cancelled" || snap.Agent.Paused || requests.Load() != 2 {
		t.Fatal(snap, requests.Load())
	}
	controlAgent(t, c, "stop", a.AgentID, "stop-stream") // retry must target only the cancelled old Task
	after, _ := c.Store.Snapshot(context.Background(), a.AgentID)
	if after.Agent.Paused {
		t.Fatal("old stop paused a new task")
	}
	page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range page.Events {
		if e.Kind == "model.repair" {
			t.Fatal("stop triggered model repair")
		}
		if e.Kind == "model.delta" && strings.Contains(string(e.Payload), "partial note") {
			found = true
		}
	}
	if !found {
		t.Fatal("partial stream lost")
	}
}

func TestStoppingOneAgentDoesNotCancelAnother(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{}, 1)
	defer close(release)
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		input := r.Messages[len(r.Messages)-1].Content[0].Text
		entered <- input
		if input == "stop me" {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, e := range fixture.Answer("final", "independent") {
			if err := emit(e); err != nil {
				return err
			}
		}
		return nil
	})
	c := loopCore(t, adapter)
	a := createWork(t, c, "stop me")
	b := createWork(t, c, "keep running")
	runCore(t, c)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("agents did not enter")
		}
	}
	controlAgent(t, c, "stop", a.AgentID, "only-a")
	waitTask(t, c, a, "cancelled")
	snap, err := c.Store.Snapshot(context.Background(), b.AgentID)
	if err != nil || snap.Agent.State != "running" || snap.Agent.Paused {
		t.Fatal(snap, err)
	}
	// Use a buffered release signal instead of ending the other Agent's context.
	release <- struct{}{}
	waitTask(t, c, b, "completed")
}

func TestCancellingWaitsForToolAndPreservesEffectsSkipsRemainingCalls(t *testing.T) {
	entered, noticed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls, skipped atomic.Int32
	events := fixture.Call("first", "effect", "{}")
	events = append(events[:len(events)-1], fixture.Call("second", "must_skip", "{}")...)
	script := fixture.New(fixture.Step{Events: events}, fixture.Step{Events: fixture.Answer("final", "resumed"), Check: func(r model.Request) error {
		results := map[string]model.Block{}
		for _, m := range r.Messages {
			if m.Role == "tool" {
				for _, b := range m.Content {
					results[b.CallID] = b
				}
			}
		}
		if results["first"].Text != "effect saved" || !results["second"].IsError || !strings.Contains(results["second"].Text, "not executed") {
			return fmt.Errorf("missing settled or skipped results")
		}
		return nil
	}})
	c := loopCore(t, script, testTool{"effect", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(noticed)
		<-release
		if err := os.WriteFile(filepath.Join(call.Cwd, "effect.txt"), []byte("one effect"), 0600); err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Content: []model.Block{{Type: "text", Text: "effect saved"}}}, nil
	}}, testTool{"must_skip", func(context.Context, tool.Call) (tool.Result, error) { skipped.Add(1); return tool.Result{}, nil }})
	a := createWork(t, c, "two calls")
	runCore(t, c)
	awaitSignal(t, entered)
	controlAgent(t, c, "stop", a.AgentID, "stop-tool")
	awaitSignal(t, noticed)
	snap := waitTask(t, c, a, "cancelling")
	if snap.Agent.State != "cancelling" || !snap.Agent.Paused {
		t.Fatal(snap)
	}
	// Resume is accepted, but cannot start a second model loop before settlement.
	res := controlAgent(t, c, "resume", a.AgentID, "resume-tool")
	snap, _ = c.Store.Snapshot(context.Background(), a.AgentID)
	if snap.Tasks[0].State != "cancelling" || snap.Tasks[1].State != "accepted" || snap.Agent.Paused || len(script.Requests()) != 1 {
		t.Fatal(snap)
	}
	once.Do(func() { close(release) })
	snap = waitTask(t, c, res, "completed")
	if snap.Tasks[0].State != "cancelled" || calls.Load() != 1 || skipped.Load() != 0 || len(script.Requests()) != 2 {
		t.Fatal(snap, calls.Load(), skipped.Load())
	}
	data, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "effect.txt"))
	if err != nil || string(data) != "one effect" {
		t.Fatal(string(data), err)
	}
}

func TestUnknownToolCancellationHoldsAcceptedResume(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	script := fixture.New(fixture.Step{Events: fixture.Call("call", "unknown", "{}")})
	c := loopCore(t, script, testTool{"unknown", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		<-release
		return tool.Result{}, ctx.Err()
	}})
	a := createWork(t, c, "unknown result")
	runCore(t, c)
	awaitSignal(t, entered)
	controlAgent(t, c, "stop", a.AgentID, "stop-unknown")
	res := controlAgent(t, c, "resume", a.AgentID, "accepted-resume")
	once.Do(func() { close(release) })
	var snap protocol.Snapshot
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		snap, _ = c.Store.Snapshot(context.Background(), a.AgentID)
		if snap.Agent.State == "unknown" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap.Agent.State != "unknown" || snap.Tasks[0].State != "cancelling" || snap.Agent.Paused || len(snap.Tasks) != 2 || snap.Tasks[1].ID != res.TaskID || snap.Tasks[1].State != "accepted" {
		t.Fatal(snap)
	}
	req, _ := protocol.NewRequest("agent.resume", "unsafe-resume", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(context.Background(), req); err == nil {
		t.Fatal("unknown resumed")
	}
	submitWork(t, c, a.AgentID, "new ID")
	if w, err := c.Store.ClaimNext(context.Background(), a.AgentID); err != nil || w != nil {
		t.Fatal(w, err)
	}
	if calls.Load() != 1 || len(script.Requests()) != 1 {
		t.Fatal(calls.Load())
	}
}

func TestStopWhileWaitingKeepsAcceptedBackgroundProcess(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprint(pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("tmux required")
				}
			}
			manager, remote, root := remoteRunner(t)
			// Private runner and tmux socket; cleanup resolves only this test's records.
			t.Cleanup(func() {
				dirs, _ := os.ReadDir(root)
				for _, dir := range dirs {
					state, err := manager.Inspect(context.Background(), dir.Name())
					if err != nil {
						continue
					}
					if state.Status == "running" || state.Status == "unknown" {
						_, _ = manager.Signal(context.Background(), protocol.NewID("cleanup"), process.SignalParams{ID: state.ID, Signal: "KILL"})
						_, _ = manager.Wait(context.Background(), state.ID, time.Second)
					}
					if state.Terminal != nil {
						_ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").Run()
					}
				}
			})
			var processID string
			started := make(chan struct{})
			adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
				command := "printf x >> marker; printf ready; read value; printf 'got:%s' \"$value\""
				if pty {
					command = "stty -echo; " + command
				}
				args, _ := json.Marshal(map[string]any{"command": command, "pty": pty, "wait_ms": 30000})
				for _, d := range fixture.Call("exec", "exec_command", string(args)) {
					if err := emit(d); err != nil {
						return err
					}
				}
				return nil
			})
			c := loopCore(t, adapter)
			base := tool.ProcessExecutors(remote)[0]
			wrapped := testTool{"exec_command", func(ctx context.Context, call tool.Call) (tool.Result, error) {
				processID = call.ID
				close(started)
				return base.Execute(ctx, call)
			}}
			var err error
			c.Tools, err = tool.NewRegistry(wrapped)
			if err != nil {
				t.Fatal(err)
			}
			a := createWork(t, c, "long process")
			runCore(t, c)
			awaitSignal(t, started)
			// The durable start is acknowledged before stop: wait until the command
			// has produced output, then allow the tool to enter read-only observation.
			ready := false
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				state, e := remote.Inspect(context.Background(), processID)
				if e == nil && state.Status == "running" {
					stream := "stdout"
					if pty {
						stream = "pty"
					}
					out, e := remote.Read(context.Background(), processID, stream, 0, 1024)
					if e == nil && strings.Contains(out.Text, "ready") {
						ready = true
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("process did not become ready")
			}
			controlAgent(t, c, "stop", a.AgentID, "stop-observer")
			snap := waitTask(t, c, a, "cancelled")
			if !snap.Agent.Paused {
				t.Fatal(snap)
			}
			messages, _, err := c.Store.ModelContext(context.Background(), a.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range messages {
				if m.Role == "tool" {
					var r tool.ProcessResult
					if json.Unmarshal([]byte(m.Content[0].Text), &r) == nil && r.ID == processID && r.ObservationInterrupted {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("accepted process identity lost", messages)
			}
			state, err := remote.Inspect(context.Background(), processID)
			if err != nil || state.Status != "running" {
				t.Fatal("stop killed background process", state, err)
			}
			if _, err := remote.Input(context.Background(), "finish", process.InputParams{ID: processID, Data: []byte("hello\n")}); err != nil {
				t.Fatal(err)
			}
			state, err = remote.Wait(context.Background(), processID, 3*time.Second)
			if err != nil || state.Status != "exited" {
				t.Fatal(state, err)
			}
			data, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker"))
			if err != nil || string(data) != "x" {
				t.Fatal(string(data), err)
			}
		})
	}
}
