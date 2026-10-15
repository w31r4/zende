package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type testTool struct {
	name    string
	execute func(context.Context, tool.Call) (tool.Result, error)
}

func (t testTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{Tool: model.Tool{Name: t.name, Description: "Test-only tool", InputSchema: json.RawMessage(`{"type":"object"}`)}, Version: "fixture-1", Snippet: "Test contract"}
}
func (t testTool) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	return t.execute(ctx, call)
}

type adapterFunc func(context.Context, model.Request, func(model.Delta) error) error

func (f adapterFunc) Stream(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
	return f(ctx, r, emit)
}

func loopCore(t *testing.T, adapter model.Adapter, executors ...tool.Executor) *Core {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "test", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	c.Tools, err = tool.NewRegistry(executors...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func createWork(t *testing.T, c *Core, input string) protocol.Accepted {
	t.Helper()
	p := protocol.CreateAgent{Cwd: c.Config.Config.Cwd, Input: input}
	req, _ := protocol.NewRequest("agent.create", protocol.NewID("op"), p)
	a, err := c.Store.Create(context.Background(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func submitWork(t *testing.T, c *Core, id, input string) protocol.Accepted {
	t.Helper()
	p := protocol.Submit{AgentID: id, Input: input}
	req, _ := protocol.NewRequest("agent.submit", protocol.NewID("op"), p)
	a, err := c.Store.Submit(context.Background(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func runCore(t *testing.T, c *Core) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	})
	return cancel, done
}
func waitTask(t *testing.T, c *Core, a protocol.Accepted, state string) protocol.Snapshot {
	t.Helper()
	var snap protocol.Snapshot
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var err error
		snap, err = c.Store.Snapshot(context.Background(), a.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range snap.Tasks {
			if task.ID == a.TaskID && task.State == state {
				if _, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID}); err != nil {
					t.Fatalf("inconsistent recovery journal for %s: %v", a.TaskID, err)
				}
				return snap
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task did not become %s: %+v", state, snap)
	return snap
}

func TestLoopTwoToolRoundsErrorFeedbackAndPersistence(t *testing.T) {
	var calls []tool.Call
	script := fixture.New(
		fixture.Step{Events: fixture.Call("first", "echo", `{"text":`, `"hello"}`)},
		fixture.Step{Events: fixture.Call("second", "fail", `{}`), Check: func(r model.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if last.Role != "tool" || last.Content[0].CallID != "first" || last.Content[0].Text != "hello" {
				return fmt.Errorf("missing first tool result")
			}
			return nil
		}},
		fixture.Step{Events: fixture.Answer("final", "finished with reported tool error"), Check: func(r model.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if last.Role != "tool" || !last.Content[0].IsError || last.Content[0].CallID != "second" || !strings.Contains(last.Content[0].Text, "test_failure") {
				return fmt.Errorf("missing tool error")
			}
			return nil
		}},
	)
	c := loopCore(t, script,
		testTool{"echo", func(_ context.Context, call tool.Call) (tool.Result, error) {
			calls = append(calls, call)
			return tool.Result{Content: []model.Block{{Type: "text", Text: "hello"}}}, nil
		}},
		testTool{"fail", func(_ context.Context, call tool.Call) (tool.Result, error) {
			calls = append(calls, call)
			return tool.Result{}, protocol.E("test_failure", "expected test failure")
		}},
	)
	a := createWork(t, c, "perform two rounds")
	runCore(t, c)
	snap := waitTask(t, c, a, "completed")
	if snap.Agent.State != "idle" || snap.Tasks[0].Result != "finished with reported tool error" {
		t.Fatal(snap)
	}
	requests := script.Requests()
	if len(requests) != 3 || len(calls) != 2 || calls[0].ID == calls[1].ID || calls[0].AgentID != a.AgentID || calls[0].TaskID != a.TaskID {
		t.Fatal(requests, calls)
	}
	messages, _, err := c.Store.ModelContext(context.Background(), a.AgentID)
	if err != nil || len(messages) != 6 || messages[0].Content[0].Text != "perform two rounds" {
		t.Fatal(messages, err)
	}
	page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	starts, results := 0, 0
	for _, e := range page.Events {
		if e.Kind == "tool.started" {
			starts++
		}
		if e.Kind == "tool.completed" {
			results++
		}
	}
	if starts != 2 || results != 2 {
		t.Fatal(starts, results)
	}
	// A second handle proves the messages and final are durable, not loop memory.
	other, err := store.Open(filepath.Join(c.Config.Config.Cwd, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	saved, _, err := other.ModelContext(context.Background(), a.AgentID)
	if err != nil || len(saved) != len(messages) {
		t.Fatal(saved, err)
	}
}

func TestSameAgentSerialDifferentAgentsConcurrent(t *testing.T) {
	entered := make(chan string, 4)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var active, peak atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		text := r.Messages[len(r.Messages)-1].Content[0].Text
		entered <- text
		if text != "queued" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, event := range fixture.Answer("final", "done "+text) {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c := loopCore(t, adapter)
	a := createWork(t, c, "first")
	b := submitWork(t, c, a.AgentID, "queued")
	other := createWork(t, c, "other")
	runCore(t, c)
	seen := map[string]bool{}
	for range 2 {
		select {
		case text := <-entered:
			seen[text] = true
		case <-time.After(3 * time.Second):
			t.Fatal("agents did not run concurrently")
		}
	}
	if !seen["first"] || !seen["other"] || peak.Load() != 2 {
		t.Fatal(seen, peak.Load())
	}
	snap, err := c.Store.Snapshot(context.Background(), a.AgentID)
	if err != nil || snap.Tasks[1].State != "accepted" {
		t.Fatal(snap, err)
	}
	messages, _, err := c.Store.ModelContext(context.Background(), a.AgentID)
	if err != nil || len(messages) != 1 {
		t.Fatal("queued input leaked into active context", messages, err)
	}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("second scheduler accepted")
	}
	once.Do(func() { close(release) })
	waitTask(t, c, a, "completed")
	waitTask(t, c, b, "completed")
	waitTask(t, c, other, "completed")
	select {
	case text := <-entered:
		if text != "queued" {
			t.Fatal(text)
		}
	case <-time.After(time.Second):
		t.Fatal("queue not consumed")
	}
}

func TestStreamFailuresNeverDispatchPartialCalls(t *testing.T) {
	for _, test := range []struct {
		name    string
		events  []model.Delta
		failure error
	}{
		{"partial", []model.Delta{{Kind: model.CallStart, CallID: "a", Name: "act"}, {Kind: model.ArgumentsDelta, CallID: "a", Text: `{"value":`}}, nil},
		{"transport-after-complete", fixture.Call("a", "act", `{}`), errors.New("secret transport details")},
		{"bad-json", fixture.Call("a", "act", `{"x":1,"x":2}`), nil},
		{"uncategorized", fixture.Answer("", "not a final"), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dispatched atomic.Int32
			c := loopCore(t, fixture.New(fixture.Step{Events: test.events, Error: test.failure}), testTool{"act", func(context.Context, tool.Call) (tool.Result, error) { dispatched.Add(1); return tool.Result{}, nil }})
			a := createWork(t, c, "work")
			runCore(t, c)
			waitTask(t, c, a, "failed")
			if dispatched.Load() != 0 {
				t.Fatal("partial response executed")
			}
			page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(page)
			if strings.Contains(string(data), "secret transport details") {
				t.Fatal("transport error leaked")
			}
			found := false
			for _, e := range page.Events {
				found = found || e.Kind == "model.delta"
			}
			if !found {
				t.Fatal("raw deltas lost")
			}
		})
	}
}

func TestUnknownToolOutcomeHoldsAgentQueue(t *testing.T) {
	script := fixture.New(fixture.Step{Events: fixture.Call("a", "uncertain", `{}`)})
	c := loopCore(t, script, testTool{"uncertain", func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{}, protocol.E("transport_unknown", "effect cannot be confirmed")
	}})
	a := createWork(t, c, "work")
	submitWork(t, c, a.AgentID, "do not run yet")
	runCore(t, c)
	snap := waitTask(t, c, a, "unknown")
	if snap.Agent.State != "unknown" || snap.Tasks[1].State != "accepted" || len(script.Requests()) != 1 {
		t.Fatal(snap)
	}
}

func TestClientDetachDoesNotCancelModelLoop(t *testing.T) {
	c, client, stopServer, serverDone := serveTest(t)
	defer func() { stopServer(); <-serverDone }()
	entered, release := make(chan struct{}), make(chan struct{})
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, event := range fixture.Answer("final", "survived detach") {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "test", Protocol: "fixture", NativeCategories: true}, nil
	})
	runCore(t, c)
	p := protocol.CreateAgent{Cwd: c.Config.Config.Cwd, Input: "persist"}
	req, _ := protocol.NewRequest("agent.create", "detach-work", p)
	clientCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var accepted protocol.Accepted
	if err := client.Call(clientCtx, req, &accepted); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model was not started")
	}
	close(release)
	waitTask(t, c, accepted, "completed")
}

func TestUnknownToolAndPanicIsolation(t *testing.T) {
	for _, kind := range []string{"unregistered", "panic"} {
		t.Run(kind, func(t *testing.T) {
			script := fixture.New(fixture.Step{Events: fixture.Call("call", "missing", `{}`)}, fixture.Step{Events: fixture.Answer("final", "handled error")})
			var executors []tool.Executor
			if kind == "panic" {
				executors = append(executors, testTool{"missing", func(context.Context, tool.Call) (tool.Result, error) { panic("do not publish panic details") }})
			}
			c := loopCore(t, script, executors...)
			a := createWork(t, c, "work")
			runCore(t, c)
			if kind == "panic" {
				waitTask(t, c, a, "unknown")
				if len(script.Requests()) != 1 {
					t.Fatal("panic continued model loop")
				}
				other := createWork(t, c, "independent work")
				waitTask(t, c, other, "completed")
			} else {
				waitTask(t, c, a, "completed")
				requests := script.Requests()
				last := requests[1].Messages[len(requests[1].Messages)-1]
				if !last.Content[0].IsError || !strings.Contains(last.Content[0].Text, "tool_not_found") {
					t.Fatal(last)
				}
			}
		})
	}
}
