package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type selectionFixture struct {
	adapters map[string]model.Adapter
}

func TestSelectionRejectsIncompatibleHistoryWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	cfg := resource.Defaults(c.Config.Config.Cwd)
	cfg.Connections["local"] = resource.Connection{Protocol: "responses", BaseURL: "https://test.invalid/v1", Credential: "env:UNRESOLVED_TEST_KEY", Models: []resource.ModelConfig{{ID: "model", ContextWindow: 8192, MaxOutput: 1024}}}
	resolver, err := provider.New(cfg, func(string) string { t.Error("selection resolved credentials"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	c.Models, c.RestoreModels = resolver, resolver
	a := createWork(t, c, "keep all history")
	if _, err := c.Store.Append(ctx, a.AgentID, a.TaskID, "context.message", store.ContextMessage{Message: model.Message{Role: "assistant", Protocol: "foreign", Opaque: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	before, _ := c.Store.Snapshot(ctx, a.AgentID)
	history, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	name := "local/model"
	req, _ := protocol.NewRequest("agent.select", "incompatible", protocol.SelectAgent{AgentID: a.AgentID, Model: &name})
	if _, err := c.Handle(ctx, req); !isFailure(err, "protocol_mismatch") {
		t.Fatal(err)
	}
	after, _ := c.Store.Snapshot(ctx, a.AgentID)
	events, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(history, events) {
		t.Fatal("incompatible selection changed saved work")
	}
}

func TestRequestSettingsJournalRejectsUnversionedChanges(t *testing.T) {
	for _, kind := range []string{"missing", "cwd", "model", "binding", "repair-budget", "revision-regression", "model-revision-regression"} {
		t.Run(kind, func(t *testing.T) {
			j := &recoveryJournal{}
			add := func(kind string, value any) error {
				raw, _ := json.Marshal(value)
				return j.add(protocol.Event{Kind: kind, Payload: raw})
			}
			base := taskSettings{Version: 2, Cwd: "/original", Model: "old", Protocol: "fixture", Binding: json.RawMessage(`{"version":1}`), SelectionRevision: 5, ModelRevision: 3}
			if err := add("task.config", base); err != nil {
				t.Fatal(err)
			}
			next := base
			switch kind {
			case "cwd":
				next.Cwd = "/changed"
			case "model":
				next.Model = "new"
				next.SelectionRevision++
			case "binding":
				next.Binding = json.RawMessage(`{"version":2}`)
				next.SelectionRevision++
			case "repair-budget":
				next.FormatRepairs++
				next.SelectionRevision++
				next.ModelRevision++
			case "revision-regression":
				next.SelectionRevision--
			case "model-revision-regression":
				next.ModelRevision--
			}
			r := requestRecord{ID: "next", Model: next.Model, Protocol: next.Protocol, Settings: &next, RepairLimit: next.FormatRepairs}
			if kind == "missing" {
				r.Settings = nil
			}
			if err := add("model.started", r); !isFailure(err, "recovery_invalid") {
				t.Fatal(kind, err)
			}
		})
	}
}

type selectedFixture struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func (r selectionFixture) Resolve(ctx context.Context, name string) (model.Binding, error) {
	return r.ResolveSelection(ctx, name, "", "")
}
func (r selectionFixture) ResolveSelection(ctx context.Context, name, effort, version string) (model.Binding, error) {
	if err := ctx.Err(); err != nil {
		return model.Binding{}, err
	}
	adapter := r.adapters[name]
	if adapter == nil || effort != "" && effort != "high" || version != "" && version != "fixture-catalog" {
		return model.Binding{}, protocol.E("model_unavailable", "fixture selection is unavailable")
	}
	raw, _ := json.Marshal(selectedFixture{name, effort})
	return model.Binding{Adapter: adapter, Model: name, Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096, Snapshot: raw}, nil
}
func (r selectionFixture) Restore(ctx context.Context, raw json.RawMessage) (model.Binding, error) {
	var p selectedFixture
	if err := protocol.StrictDecode(raw, &p); err != nil {
		return model.Binding{}, err
	}
	return r.ResolveSelection(ctx, p.Model, p.Effort, "")
}

func choose(t *testing.T, c *Core, p protocol.SelectAgent) protocol.Accepted {
	t.Helper()
	req, _ := protocol.NewRequest("agent.select", protocol.NewID("selection"), p)
	value, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.Accepted)
}

func TestSelectionTakesEffectAfterOldResponseAndCalls(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan model.Request, 1), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	oldAdapter := adapterFunc(func(ctx context.Context, req model.Request, emit func(model.Delta) error) error {
		entered <- req
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		for _, d := range fixture.Call("old-call", "inspect", `{}`) {
			if err := emit(d); err != nil {
				return err
			}
		}
		return nil
	})
	newDir := t.TempDir()
	newAdapter := fixture.New(
		fixture.Step{Events: fixture.Call("new-call", "inspect", `{}`), Check: func(req model.Request) error {
			last := req.Messages[len(req.Messages)-1]
			if req.Model != "new" || last.Role != "developer" || !strings.Contains(last.Content[0].Text, newDir) || !strings.Contains(last.Content[0].Text, `"effort":"high"`) {
				return fmt.Errorf("missing selected runtime settings: %+v", req)
			}
			if len(req.Messages) < 4 || req.Messages[len(req.Messages)-2].Role != "tool" {
				return fmt.Errorf("selection was applied before the old tool result")
			}
			return nil
		}},
		fixture.Step{Events: fixture.Answer("final", "done")},
	)
	calls := make(chan tool.Call, 2)
	c := loopCore(t, oldAdapter, testTool{"inspect", func(_ context.Context, call tool.Call) (tool.Result, error) {
		calls <- call
		return tool.Result{Content: []model.Block{{Type: "text", Text: call.Cwd}}}, nil
	}})
	c.Config.Config.Model = "old"
	resolver := selectionFixture{map[string]model.Adapter{"old": oldAdapter, "new": newAdapter}}
	c.Models, c.RestoreModels = resolver, resolver
	a := createWork(t, c, "preserve original input")
	runCore(t, c)
	var original model.Request
	select {
	case original = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old request did not start")
	}
	history, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	name, effort := "new", "high"
	accepted := choose(t, c, protocol.SelectAgent{AgentID: a.AgentID, Cwd: &newDir, Model: &name, Effort: &effort})
	// Acceptance is durable while the old response has not finished.
	messages, _, _ := c.Store.ModelContext(ctx, a.AgentID)
	if len(messages) != 1 || messages[0].Content[0].Text != "preserve original input" {
		t.Fatal("selection changed in-flight model context", messages)
	}
	once.Do(func() { close(release) })
	waitTask(t, c, a, "completed")
	oldCall, newCall := <-calls, <-calls
	if oldCall.Cwd != c.Config.Config.Cwd || newCall.Cwd != newDir {
		t.Fatal("tool retargeted across request boundary", oldCall, newCall)
	}
	requests := newAdapter.Requests()
	if len(requests) != 2 || original.System != requests[0].System || !reflect.DeepEqual(original.Messages, requests[0].Messages[:len(original.Messages)]) {
		t.Fatal("replaced earlier prompt or messages")
	}
	events, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if !reflect.DeepEqual(history.Events, events.Events[:len(history.Events)]) {
		t.Fatal("rewrote history")
	}
	var records []requestRecord
	for _, e := range events.Events {
		if e.Kind == "model.started" {
			var r requestRecord
			if err := json.Unmarshal(e.Payload, &r); err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
		}
	}
	if len(records) != 3 || records[0].Settings.Cwd != c.Config.Config.Cwd || records[0].Settings.SelectionRevision != 0 || records[1].Settings.Cwd != newDir || records[1].Settings.SelectionRevision != accepted.Selection.Revision || records[2].Settings.Effort != "high" {
		t.Fatal(records)
	}
	j, err := readRecoveryJournal(ctx, c.Store, a.AgentID, a.TaskID)
	if err != nil || j.Settings.Cwd != newDir || j.Settings.Model != "new" {
		t.Fatal(j, err)
	}
}

func TestExplicitSelectionRunsWithoutProfileDefault(t *testing.T) {
	script := fixture.New(fixture.Step{Events: fixture.Answer("final", "selected")})
	c := loopCore(t, nil)
	resolver := selectionFixture{map[string]model.Adapter{"chosen": script}}
	c.Models, c.RestoreModels = nil, resolver
	a := createWork(t, c, "waiting for selection")
	runCore(t, c)
	// No default: accepted input stays queued instead of failing.
	time.Sleep(75 * time.Millisecond)
	snapshot, _ := c.Store.Snapshot(context.Background(), a.AgentID)
	if snapshot.Tasks[0].State != "accepted" || len(script.Requests()) != 0 {
		t.Fatal(snapshot)
	}
	name := "chosen"
	choose(t, c, protocol.SelectAgent{AgentID: a.AgentID, Model: &name})
	waitTask(t, c, a, "completed")
	if len(script.Requests()) != 1 {
		t.Fatal("selected task did not run")
	}
}

func TestSelectionRecoveryUsesDispatchedRequestSnapshot(t *testing.T) {
	for _, boundary := range []string{"before-next-request", "after-next-request"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "recovered")})
			calls := make(chan tool.Call, 1)
			c := loopCore(t, nil, testTool{"inspect", func(_ context.Context, call tool.Call) (tool.Result, error) {
				calls <- call
				return tool.Result{Content: []model.Block{{Type: "text", Text: "original effect"}}}, nil
			}})
			resolver := selectionFixture{map[string]model.Adapter{"old": script, "new": script}}
			c.Config.Config.Model = "old"
			c.Models, c.RestoreModels = resolver, resolver
			a := createWork(t, c, "work survives")
			if _, err := c.Store.ClaimNext(ctx, a.AgentID); err != nil {
				t.Fatal(err)
			}
			binding, _ := resolver.Resolve(ctx, "old")
			settings := taskSettings{Version: 2, Selection: "old", Cwd: c.Config.Config.Cwd, Model: "old", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096, FormatRepairs: c.Config.Config.Limits.FormatRepairs, Binding: binding.Snapshot}
			if err := c.Store.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "task.config", settings); err != nil {
				t.Fatal(err)
			}
			if err := c.Store.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "prompt.loaded", Instruction{Source: "test", Version: "1", Body: "keep original prompt"}); err != nil {
				t.Fatal(err)
			}
			record := func(id string, s taskSettings) {
				t.Helper()
				_, cursor, _ := c.Store.ModelContext(ctx, a.AgentID)
				err := c.Store.BeginModelRequest(ctx, a.AgentID, a.TaskID, s.SelectionRevision, cursor, "", func(cursor int64) any {
					return requestRecord{ID: id, Model: s.Model, Protocol: s.Protocol, Cursor: cursor, Tools: c.Tools.Descriptors(), MaxOutputTokens: s.MaxOutputTokens, RepairLimit: s.FormatRepairs, Settings: &s}
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			record("original", settings)
			newDir, name := t.TempDir(), "new"
			chosen := choose(t, c, protocol.SelectAgent{AgentID: a.AgentID, Cwd: &newDir, Model: &name})
			expectedCwd, requestID := settings.Cwd, "original"
			if boundary == "after-next-request" {
				if err := c.Store.RecordResponse(ctx, a.AgentID, a.TaskID, requestID, model.Response{Output: []model.PublicMessage{{Category: "commentary", Text: "continue"}}}); err != nil {
					t.Fatal(err)
				}
				binding, _ = resolver.Resolve(ctx, name)
				settings.Cwd, settings.Model, settings.Selection, settings.Binding = newDir, name, name, binding.Snapshot
				settings.SelectionRevision, settings.ModelRevision = chosen.Selection.Revision, chosen.Selection.ModelRevision
				requestID, expectedCwd = "next", newDir
				record(requestID, settings)
				// Even a later desired cwd cannot retarget this request's call.
				laterDir := t.TempDir()
				choose(t, c, protocol.SelectAgent{AgentID: a.AgentID, Cwd: &laterDir})
			}
			call := model.ToolCall{ID: "pending", Name: "inspect", Arguments: json.RawMessage(`{}`)}
			response := model.Response{Calls: []model.ToolCall{call}, Messages: []model.Message{{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: call.ID, Name: call.Name, Arguments: call.Arguments}}}}}
			if err := c.Store.RecordResponse(ctx, a.AgentID, a.TaskID, requestID, response); err != nil {
				t.Fatal(err)
			}
			if err := c.Store.HoldInterrupted(ctx); err != nil {
				t.Fatal(err)
			}
			j, err := readRecoveryJournal(ctx, c.Store, a.AgentID, a.TaskID)
			if err != nil || j.Settings.Cwd != expectedCwd {
				t.Fatal(j, err)
			}
			runCore(t, c)
			waitTask(t, c, a, "completed")
			if call := <-calls; call.Cwd != expectedCwd {
				t.Fatal("recovery retargeted old call", call)
			}
			requests := script.Requests()
			if len(requests) != 1 || requests[0].Model != "new" || requests[0].System != "keep original prompt" {
				t.Fatal(requests)
			}
		})
	}
}
