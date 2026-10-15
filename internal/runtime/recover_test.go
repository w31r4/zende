package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

type savedFixture struct {
	adapter  model.Adapter
	restores atomic.Int32
}

func (s *savedFixture) Resolve(context.Context, string) (model.Binding, error) {
	return model.Binding{Adapter: s.adapter, Model: "test", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096, Snapshot: json.RawMessage(`{"fixture":1}`)}, nil
}
func (s *savedFixture) Restore(ctx context.Context, raw json.RawMessage) (model.Binding, error) {
	if string(raw) != `{"fixture":1}` {
		return model.Binding{}, fmt.Errorf("wrong snapshot")
	}
	s.restores.Add(1)
	return s.Resolve(ctx, "")
}

func savedWork(t *testing.T, c *Core, adapter model.Adapter) (protocol.Accepted, func(string, any), *savedFixture) {
	t.Helper()
	r := &savedFixture{adapter: adapter}
	c.Models = r
	a := createWork(t, c, "original input")
	if _, err := c.Store.ClaimNext(context.Background(), a.AgentID); err != nil {
		t.Fatal(err)
	}
	appendEvent := func(kind string, payload any) {
		t.Helper()
		if err := c.Store.AppendTaskEvent(context.Background(), a.AgentID, a.TaskID, kind, payload); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := r.Resolve(context.Background(), "")
	appendEvent("task.config", taskSettings{Version: 1, Cwd: c.Config.Config.Cwd, Selection: "fixture/test", Model: b.Model, Protocol: b.Protocol, NativeCategories: b.NativeCategories, MaxOutputTokens: b.MaxOutputTokens, FormatRepairs: 1, Binding: b.Snapshot})
	appendEvent("prompt.loaded", Instruction{Source: "fixture", Version: "1", Body: "original system prompt"})
	appendEvent("model.started", requestRecord{ID: "request-original", Model: b.Model, Protocol: b.Protocol, MaxOutputTokens: b.MaxOutputTokens, Tools: c.Tools.Descriptors(), RepairLimit: 1})
	return a, appendEvent, r
}

func reopenCore(t *testing.T, c *Core) *Core {
	t.Helper()
	if err := c.Store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(c.Config.Config.Cwd, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	next := New(s, c.Config)
	next.Models = c.Models
	next.RestoreModels = c.RestoreModels
	next.Tools = c.Tools
	return next
}

func TestRecoveryContinuesOriginalTaskAtDispatchBoundaries(t *testing.T) {
	for _, stage := range []string{"planned", "prepared", "dispatched", "completed"} {
		t.Run(stage, func(t *testing.T) {
			_, remote, _ := remoteRunner(t)
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "recovered"), Check: func(req model.Request) error {
				if req.System != "original system prompt" || req.ID == "request-original" || len(req.Messages) != 3 || req.Messages[2].Content[0].CallID != "original-call" {
					return fmt.Errorf("lost context or reused request: %+v", req)
				}
				return nil
			}})
			c := loopCore(t, script, tool.ProcessExecutors(remote)...)
			a, appendEvent, _ := savedWork(t, c, script)
			call := model.ToolCall{ID: "original-call", Name: "exec_command", Arguments: json.RawMessage(`{"command":"printf x >> marker; printf done","wait_ms":3000}`)}
			journalResponse(t, c, a, call)
			op := invocation{SchemaVersion: 2, ID: "operation-original", RequestID: "request-original", Call: call, Version: "2"}
			if stage != "planned" {
				appendEvent("tool.started", op)
			}
			if stage == "dispatched" || stage == "completed" {
				appendEvent("tool.dispatching", op)
				executor, _, _ := c.Tools.Lookup(call.Name)
				result, err := executor.Execute(context.Background(), tool.Call{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: call.Arguments})
				if err != nil {
					t.Fatal(err)
				}
				if stage == "completed" {
					message, err := toolMessage(call, result, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := c.Store.RecordToolResult(context.Background(), a.AgentID, a.TaskID, op.RequestID, toolOutcome{op, result, nil}, message); err != nil {
						t.Fatal(err)
					}
				}
			}
			c = reopenCore(t, c)
			runCore(t, c)
			snap := waitTask(t, c, a, "completed")
			if len(snap.Tasks) != 1 || snap.Tasks[0].Result != "recovered" {
				t.Fatal(snap)
			}
			bytes, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker"))
			if err != nil || string(bytes) != "x" {
				t.Fatal("effect replayed", string(bytes), err)
			}
			page, _ := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
			var starts, dispatches, results, proofs int
			for _, e := range page.Events {
				switch e.Kind {
				case "tool.started":
					starts++
				case "tool.dispatching":
					dispatches++
				case "tool.completed":
					results++
				case "tool.verified":
					proofs++
				}
				if stage != "planned" && e.Kind == "tool.completed" {
					var got toolOutcome
					if json.Unmarshal(e.Payload, &got) != nil || got.Invocation.ID != op.ID {
						t.Fatal(e)
					}
				}
			}
			wantProof := 0
			if stage == "dispatched" {
				wantProof = 1
			}
			if starts != 1 || dispatches != 1 || results != 1 || proofs != wantProof {
				t.Fatal(starts, dispatches, results, proofs)
			}
		})
	}
}

func TestRecoveryPatchReceiptContinuesWithoutReapplying(t *testing.T) {
	for _, stage := range []string{"prepared", "saved-success", "saved-conflict", "missing-receipt", "different-version"} {
		t.Run(stage, func(t *testing.T) {
			executor := tool.DurablePatchExecutor(filepath.Join(t.TempDir(), "patches"))
			wantFailure := stage == "saved-conflict"
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "patch recovered"), Check: func(req model.Request) error {
				last := req.Messages[len(req.Messages)-1]
				if last.Role != "tool" || last.Content[0].CallID != "original-patch" || last.Content[0].IsError != wantFailure {
					return fmt.Errorf("lost original patch result: %+v", last)
				}
				if !wantFailure && !strings.Contains(last.Content[0].Text, `"status":"applied"`) {
					return fmt.Errorf("patch was replayed instead of recovering its successful receipt: %+v", last)
				}
				return nil
			}})
			c := loopCore(t, script, executor)
			a, appendEvent, _ := savedWork(t, c, script)
			args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: marker\n+once\n*** End Patch"})
			call := model.ToolCall{ID: "original-patch", Name: "apply_patch", Arguments: args}
			journalResponse(t, c, a, call)
			op := invocation{SchemaVersion: 2, ID: "patch-effect", RequestID: "request-original", Call: call, Version: executor.Descriptor().Version}
			appendEvent("tool.started", op)
			if stage != "prepared" {
				appendEvent("tool.dispatching", op)
				if wantFailure {
					if err := os.WriteFile(filepath.Join(c.Config.Config.Cwd, "marker"), []byte("external\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "missing-receipt" {
					executor = tool.PatchExecutor() // An old execution with no durable receipt.
				}
				_, err := executor.Execute(context.Background(), tool.Call{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: args})
				if (err != nil) != wantFailure {
					t.Fatal(err)
				}
			}
			if stage == "different-version" {
				var err error
				c.Tools, err = tool.NewRegistry(testTool{"apply_patch", func(context.Context, tool.Call) (tool.Result, error) { panic("wrong tool version ran") }})
				if err != nil {
					t.Fatal(err)
				}
			}
			c = reopenCore(t, c)
			runCore(t, c)
			if stage == "missing-receipt" || stage == "different-version" {
				waitTask(t, c, a, "unknown")
				queued := submitWork(t, c, a.AgentID, "try equivalent patch with another call ID")
				// Wait for an explicit verification to finish, rather than treating
				// a momentary unknown state as proof that the scheduler stays held.
				p := protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID}
				req, _ := protocol.NewRequest("agent.recover", "recheck-patch", p)
				if _, err := c.Handle(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				held := false
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
					page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range page.Events {
						if event.Kind == "recovery.held" {
							held = true
						}
					}
					if held {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if !held || len(script.Requests()) != 0 {
					t.Fatal("unverified effect resumed", held, script.Requests())
				}
				waitTask(t, c, queued, "accepted")
			} else {
				snap := waitTask(t, c, a, "completed")
				if len(snap.Tasks) != 1 || snap.Tasks[0].Result != "patch recovered" {
					t.Fatal(snap)
				}
			}
			want := "once\n"
			if wantFailure {
				want = "external\n"
			}
			data, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker"))
			if err != nil || string(data) != want {
				t.Fatal("effect changed", string(data), err)
			}
		})
	}
}

func TestRecoveryIncompleteStreamAndSavedFinal(t *testing.T) {
	for _, stage := range []string{"partial", "final", "before-settings"} {
		t.Run(stage, func(t *testing.T) {
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "continued"), Check: func(req model.Request) error {
				if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "original input" {
					return fmt.Errorf("partial response entered context")
				}
				return nil
			}})
			c := loopCore(t, script)
			var a protocol.Accepted
			if stage == "before-settings" {
				a = createWork(t, c, "original input")
				if _, err := c.Store.ClaimNext(context.Background(), a.AgentID); err != nil {
					t.Fatal(err)
				}
			} else {
				var appendEvent func(string, any)
				a, appendEvent, _ = savedWork(t, c, script)
				if stage == "partial" {
					appendEvent("model.delta", map[string]any{"request_id": "request-original", "delta": model.Delta{Kind: model.TextDelta, MessageID: "partial", Text: "keep original fragment"}})
				} else {
					if err := c.Store.RecordResponse(context.Background(), a.AgentID, a.TaskID, "request-original", model.Response{Output: []model.PublicMessage{{Category: "final", Text: "saved final"}}}); err != nil {
						t.Fatal(err)
					}
					c.Models = nil // Completing a persisted final does not need credentials.
				}
			}
			c = reopenCore(t, c)
			runCore(t, c)
			snap := waitTask(t, c, a, "completed")
			if stage == "final" {
				if len(script.Requests()) != 0 || snap.Tasks[0].Result != "saved final" {
					t.Fatal(snap)
				}
			} else if len(script.Requests()) != 1 {
				t.Fatal(script.Requests())
			}
			messages, _, _ := c.Store.ModelContext(context.Background(), a.AgentID)
			users := 0
			for _, m := range messages {
				if m.Role == "user" {
					users++
				}
			}
			if users != 1 || len(snap.Tasks) != 1 {
				t.Fatal(users, snap)
			}
		})
	}
}

type verifiedFixtureTool struct {
	testTool
	ready  atomic.Bool
	checks atomic.Int32
}

func (t *verifiedFixtureTool) Recover(context.Context, tool.Call) (tool.Recovered, error) {
	t.checks.Add(1)
	if !t.ready.Load() {
		return tool.Recovered{}, protocol.E("operation_unknown", "fixture proof unavailable")
	}
	return tool.Recovered{Result: tool.Result{Content: []model.Block{{Type: "text", Text: "proved original effect"}}}}, nil
}

func TestExplicitRecoveryDoesNotTreatNewInputAsProof(t *testing.T) {
	var executions atomic.Int32
	executor := &verifiedFixtureTool{testTool: testTool{"effect", func(context.Context, tool.Call) (tool.Result, error) { executions.Add(1); return tool.Result{}, nil }}}
	script := fixture.New(fixture.Step{Events: fixture.Answer("final", "original complete")}, fixture.Step{Events: fixture.Answer("final", "queued complete")})
	c := loopCore(t, script, executor)
	a, appendEvent, _ := savedWork(t, c, script)
	call := model.ToolCall{ID: "model-call", Name: "effect", Arguments: json.RawMessage(`{}`)}
	journalResponse(t, c, a, call)
	op := invocation{SchemaVersion: 2, ID: "original-operation", RequestID: "request-original", Call: call, Version: "fixture-1"}
	appendEvent("tool.started", op)
	appendEvent("tool.dispatching", op)
	c = reopenCore(t, c)
	runCore(t, c)
	waitRecoveryEvent(t, c, a, "recovery.held")
	queued := submitWork(t, c, a.AgentID, "new ID is not proof")
	time.Sleep(80 * time.Millisecond)
	if len(script.Requests()) != 0 || executions.Load() != 0 || executor.checks.Load() != 1 {
		t.Fatal("ordinary input released or retried gate", script.Requests(), executions.Load(), executor.checks.Load())
	}
	executor.ready.Store(true)
	req, _ := protocol.NewRequest("agent.recover", "verify-original", protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID})
	first, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := c.Handle(context.Background(), req)
		if err != nil || first != again {
			t.Fatal(again, err)
		}
	}
	waitTask(t, c, a, "completed")
	waitTask(t, c, queued, "completed")
	if executions.Load() != 0 || executor.checks.Load() != 2 {
		t.Fatal(executions.Load(), executor.checks.Load())
	}
}

func waitRecoveryEvent(t *testing.T, c *Core, a protocol.Accepted, kind string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			if e.TaskID == a.TaskID && e.Kind == kind {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("missing recovery event", kind)
}

func TestRecoveryCancellationSkipsPreparedCallsWithoutModel(t *testing.T) {
	var executions atomic.Int32
	c := loopCore(t, nil, testTool{"effect", func(context.Context, tool.Call) (tool.Result, error) { executions.Add(1); return tool.Result{}, nil }})
	a, appendEvent, _ := savedWork(t, c, nil)
	call := model.ToolCall{ID: "original-call", Name: "effect", Arguments: json.RawMessage(`{}`)}
	journalResponse(t, c, a, call)
	appendEvent("tool.started", invocation{SchemaVersion: 2, ID: "original-operation", RequestID: "request-original", Call: call, Version: "fixture-1"})
	if err := c.Store.HoldInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("agent.stop", "stop", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	c.Models = nil
	c = reopenCore(t, c)
	runCore(t, c)
	snap := waitTask(t, c, a, "cancelled")
	if !snap.Agent.Paused || executions.Load() != 0 {
		t.Fatal(snap, executions.Load())
	}
	messages, _, _ := c.Store.ModelContext(context.Background(), a.AgentID)
	if len(messages) < 3 || !strings.Contains(messages[2].Content[0].Text, "task_cancelled") {
		t.Fatal(messages)
	}
}

func TestRecoveryDoesNotResetInterruptedRepairBudget(t *testing.T) {
	script := fixture.New()
	c := loopCore(t, script)
	a, appendEvent, _ := savedWork(t, c, script)
	appendEvent("model.output.invalid", map[string]any{"request_id": "request-original", "error": protocol.E("output_format", "missing category")})
	if err := c.Store.RecordFormatRepair(context.Background(), a.AgentID, a.TaskID, "request-original", 1, 1, "repair once"); err != nil {
		t.Fatal(err)
	}
	appendEvent("model.started", requestRecord{ID: "repair-request", Model: "test", Protocol: "fixture", MaxOutputTokens: 4096, FormatRepair: true, RepairAttempt: 1, RepairLimit: 1})
	c.Config.Config.Limits.FormatRepairs = 99
	c = reopenCore(t, c)
	runCore(t, c)
	waitTask(t, c, a, "failed")
	if len(script.Requests()) != 0 {
		t.Fatal("restarted repair budget", script.Requests())
	}
	j, err := readRecoveryJournal(context.Background(), c.Store, a.AgentID, a.TaskID)
	if err != nil || j.RepairCount != 1 || j.RepairLimit != 1 {
		t.Fatal(j, err)
	}
}

func TestRecoveryUsesAlreadyReservedRepairWithoutChargingAgain(t *testing.T) {
	script := fixture.New(fixture.Step{Events: fixture.Answer("final", "repaired after restart"), Check: func(req model.Request) error {
		if len(req.Tools) != 0 || req.Messages[len(req.Messages)-1].Role != "developer" {
			return fmt.Errorf("reserved text-only repair lost: %+v", req)
		}
		return nil
	}})
	c := loopCore(t, script, testTool{"unused", func(context.Context, tool.Call) (tool.Result, error) { panic("repair dispatched tool") }})
	a, appendEvent, _ := savedWork(t, c, script)
	appendEvent("model.output.invalid", map[string]any{"request_id": "request-original", "error": protocol.E("output_format", "missing category")})
	if err := c.Store.RecordFormatRepair(context.Background(), a.AgentID, a.TaskID, "request-original", 1, 1, "repair once"); err != nil {
		t.Fatal(err)
	}
	// Crash after reserving the attempt, before dispatching its model request.
	c.Config.Config.Limits.FormatRepairs = 0
	c = reopenCore(t, c)
	runCore(t, c)
	waitTask(t, c, a, "completed")
	j, err := readRecoveryJournal(context.Background(), c.Store, a.AgentID, a.TaskID)
	if err != nil || j.RepairCount != 1 || j.RepairOnly || len(script.Requests()) != 1 {
		t.Fatal(j, err, script.Requests())
	}
}

func TestRecoveryRejoinsRunningPipeAndPTY(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprint(pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("real tmux required")
				}
			}
			manager, remote, _ := remoteRunner(t)
			script := fixture.New(fixture.Step{Events: fixture.Call("input", "write_stdin", `{"process_id":"original-process","chars":"done\n","wait_ms":3000}`), Check: func(req model.Request) error {
				last := req.Messages[len(req.Messages)-1]
				var state tool.ProcessResult
				if len(last.Content) != 1 || json.Unmarshal([]byte(last.Content[0].Text), &state) != nil || state.ID != "original-process" || state.Status != "running" {
					return fmt.Errorf("original process not recovered: %+v", last)
				}
				return nil
			}}, fixture.Step{Events: fixture.Answer("final", "original process continued")})
			c := loopCore(t, script, tool.ProcessExecutors(remote)...)
			a, appendEvent, _ := savedWork(t, c, script)
			args, _ := json.Marshal(map[string]any{"command": "printf x >> marker; read value; printf '%s' \"$value\"", "pty": pty, "wait_ms": 0})
			call := model.ToolCall{ID: "original-call", Name: "exec_command", Arguments: args}
			journalResponse(t, c, a, call)
			op := invocation{SchemaVersion: 2, ID: "original-process", RequestID: "request-original", Call: call, Version: "2"}
			appendEvent("tool.started", op)
			appendEvent("tool.dispatching", op)
			executor, _, _ := c.Tools.Lookup(call.Name)
			if _, err := executor.Execute(context.Background(), tool.Call{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: args}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				state, err := manager.Inspect(context.Background(), op.ID)
				if err != nil {
					return
				}
				if state.Status == "running" || state.Status == "unknown" {
					_, _ = manager.Signal(context.Background(), "cleanup", process.SignalParams{ID: op.ID, Signal: "KILL"})
					_, _ = manager.Wait(context.Background(), op.ID, time.Second)
				}
				if state.Terminal != nil {
					_ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").Run()
				}
			})
			c = reopenCore(t, c)
			runCore(t, c)
			waitTask(t, c, a, "completed")
			state, err := manager.Inspect(context.Background(), op.ID)
			if err != nil || state.Status != "exited" || state.InputBytes != 5 {
				t.Fatal(state, err)
			}
			data, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker"))
			if err != nil || string(data) != "x" {
				t.Fatal(string(data), err)
			}
		})
	}
}
