package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func nativePluginInput(t *testing.T, c *Core, target protocol.ResourceTarget, kind, text string) json.RawMessage {
	t.Helper()
	catalog, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: target.AgentID, ID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range catalog.Definitions {
		if d.Kind == kind {
			raw, _ := json.Marshal(map[string]any{"resource_id": target.ID, "tool_ref": d.Ref, "arguments": map[string]string{"text": text}})
			return raw
		}
	}
	t.Fatal("missing fixture definition", kind)
	return nil
}

func nativePluginCall(t *testing.T, c *Core, target protocol.ResourceTarget, text string) tool.Call {
	t.Helper()
	a := submitWork(t, c, target.AgentID, "native plugin")
	if _, err := c.Store.ClaimNext(t.Context(), a.AgentID); err != nil {
		t.Fatal(err)
	}
	return tool.Call{ID: "original-call", AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: nativePluginInput(t, c, target, "tool", text)}
}

func nativePluginValue(t *testing.T, result tool.Result) pluginToolResult {
	t.Helper()
	var value pluginToolResult
	if len(result.Content) != 1 || len(result.Content[0].Text) > 64<<10 || json.Unmarshal([]byte(result.Content[0].Text), &value) != nil || strings.Contains(result.Content[0].Text, `"evidence"`) {
		t.Fatal("invalid native result", result)
	}
	return value
}

func nativePluginAudit(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(raw, []byte(`"method":"plugin.tool.call"`))
}

func TestPluginNativeOutputRecoveryAndLimits(t *testing.T) {
	for _, text := range []string{"small 中文", strings.Repeat("你好<&>🙂", 24000)} {
		t.Run(fmt.Sprint(len(text)), func(t *testing.T) {
			c, target, _, _ := registeredPlugin(t, "")
			call := nativePluginCall(t, c, target, text)
			executor := pluginTool{core: c}
			result, err := executor.Execute(t.Context(), call)
			if err != nil {
				t.Fatal(err)
			}
			value := nativePluginValue(t, result)
			if value.Receipt.State != "completed" || value.Receipt.OperationID != call.ID || value.Receipt.TaskID != call.TaskID || value.Receipt.Kind != "tool" {
				t.Fatal(value)
			}
			if len(text) < 32<<10 {
				if value.More || !strings.Contains(string(value.Output), text) {
					t.Fatal(value)
				}
			} else if !value.More || len(value.Output) != 0 || len(value.Preview) > 4096 || value.OutputBytes < len(text) {
				t.Fatal(value)
			}
			completion, err := c.Store.PluginCompletion(t.Context(), value.Receipt.PluginCallTarget)
			var output map[string]string
			if err != nil || json.Unmarshal(completion.Output, &output) != nil || output["text"] != text {
				t.Fatal(completion, err)
			}
			c.CloseResources()
			for range 2 {
				again, err := executor.Execute(t.Context(), call)
				if err != nil || !reflect.DeepEqual(result, again) {
					t.Fatal(again, err)
				}
				recovered, err := executor.Recover(t.Context(), call)
				if err != nil || recovered.Error != nil || !reflect.DeepEqual(result, recovered.Result) {
					t.Fatal(recovered, err)
				}
			}
			if nativePluginAudit(t) != 1 {
				t.Fatal("recovery repeated effect")
			}
		})
	}
}

func TestPluginNativeErrorsCancellationAndNoDispatch(t *testing.T) {
	for _, tc := range []struct{ mode, state, failure string }{{"call-failed", "failed", "plugin_call_failed"}, {"call-cancel", "cancelled", "plugin_cancelled"}, {"call-exit", "unknown", "operation_unknown"}} {
		t.Run(tc.state, func(t *testing.T) {
			c, target, _, _ := registeredPlugin(t, tc.mode)
			call := nativePluginCall(t, c, target, "work")
			executor := pluginTool{core: c}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type answer struct {
				result tool.Result
				err    error
			}
			done := make(chan answer, 1)
			go func() { r, err := executor.Execute(ctx, call); done <- answer{r, err} }()
			if tc.mode == "call-cancel" {
				waitPluginReceipt(t, c, protocol.PluginCallTarget{AgentID: call.AgentID, OperationID: call.ID}, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
				cancel()
			}
			var got answer
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("native call did not settle")
			}
			if !isFailure(got.err, tc.failure) {
				t.Fatal(got.err)
			}
			value := nativePluginValue(t, got.result)
			if value.Receipt.State != tc.state {
				t.Fatal(value)
			}
			if tc.state == "cancelled" && value.Receipt.ProgressSeq != 2 {
				t.Fatal("missing cancellation progress", value)
			}
			recovered, err := executor.Recover(t.Context(), call)
			if tc.state == "unknown" {
				if !isFailure(err, "operation_unknown") {
					t.Fatal(recovered, err)
				}
			} else if err != nil || !isFailure(recovered.Error, tc.failure) {
				t.Fatal(recovered, err)
			}
			if nativePluginAudit(t) != 1 {
				t.Fatal("repeated effect")
			}
		})
	}
	c, target, _, _ := registeredPlugin(t, "")
	call := nativePluginCall(t, c, target, "work")
	executor := pluginTool{core: c}
	for _, raw := range []json.RawMessage{json.RawMessage(`{"resource_id":"x"}`), nativePluginInput(t, c, target, "command", "work")} {
		bad := call
		bad.Arguments = raw
		if _, err := executor.Execute(t.Context(), bad); err == nil {
			t.Fatal("invalid input invoked plugin")
		}
	}
	recovered, err := executor.Recover(t.Context(), call)
	if err != nil || !isFailure(recovered.Error, "plugin_not_dispatched") || nativePluginAudit(t) != 0 {
		t.Fatal(recovered, err)
	}
}

func TestPluginNativeBoundsFailureDetails(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "call-failed-large")
	call := nativePluginCall(t, c, target, strings.Repeat("x", 30<<10))
	executor := pluginTool{core: c}
	result, err := executor.Execute(t.Context(), call)
	if !isFailure(err, "plugin_call_failed") {
		t.Fatal("confirmed failure became unknown due to display size", err)
	}
	value := nativePluginValue(t, result)
	if !value.More || value.Failure == nil || len(value.Failure.Message) > 4096 || len(value.Output) == 0 {
		t.Fatal(value)
	}
	completion, err := c.Store.PluginCompletion(t.Context(), value.Receipt.PluginCallTarget)
	if err != nil || completion.Error == nil || len(completion.Error.Message) != 8192 {
		t.Fatal("full error was not retained", completion, err)
	}
	c.CloseResources()
	recovered, err := executor.Recover(t.Context(), call)
	if err != nil || !isFailure(recovered.Error, "plugin_call_failed") || !reflect.DeepEqual(result, recovered.Result) || nativePluginAudit(t) != 1 {
		t.Fatal(recovered, err)
	}
}

func TestPluginNativeCallKeepsRetiredInstance(t *testing.T) {
	releaseFile := filepath.Join(t.TempDir(), "release")
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", releaseFile)
	c, target, manifest, source := registeredPlugin(t, "call-block")
	call := nativePluginCall(t, c, target, "old output")
	executor := pluginTool{core: c}
	done := make(chan error, 1)
	go func() { _, err := executor.Execute(t.Context(), call); done <- err }()
	original := waitPluginReceipt(t, c, protocol.PluginCallTarget{AgentID: call.AgentID, OperationID: call.ID}, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	stale := call
	stale.ID = "new-call-old-reference"
	if _, err := executor.Execute(t.Context(), stale); !isFailure(err, "stale_reference") {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseFile, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old call interrupted")
	}
	completed := waitPluginReceipt(t, c, original.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
	if completed.Binding != original.Binding {
		t.Fatal("old call changed version")
	}
	newCall := call
	newCall.ID = "new-version"
	newCall.Arguments = nativePluginInput(t, c, target, "tool", "new output")
	result, err := executor.Execute(t.Context(), newCall)
	if err != nil {
		t.Fatal(err)
	}
	value := nativePluginValue(t, result)
	if value.Receipt.Binding.PluginVersion != "2" || value.Receipt.Binding.InstanceID == original.Binding.InstanceID || nativePluginAudit(t) != 2 {
		t.Fatal(value)
	}
}

func TestPluginNativePersistenceFailureCannotBecomeModelSuccess(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	call := nativePluginCall(t, c, target, "work")
	pluginTestSQL(t, c, `CREATE TRIGGER fail_plugin_result BEFORE UPDATE ON plugin_calls WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'injected result failure'); END`)
	executor := pluginTool{core: c}
	if _, err := executor.Execute(t.Context(), call); !isFailure(err, "operation_unknown") {
		t.Fatal(err)
	}
	if _, err := executor.Recover(t.Context(), call); err == nil {
		t.Fatal("uncommitted result recovered")
	}
	pluginTestSQL(t, c, `DROP TRIGGER fail_plugin_result`)
	c.CloseResources()
	if err := c.Store.InterruptPluginCalls(t.Context(), "replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Recover(t.Context(), call); err == nil {
		t.Fatal("unknown result recovered")
	}
	if nativePluginAudit(t) != 1 {
		t.Fatal("persistence failure repeated effect")
	}
}

func TestPluginNativeRejectsCorruptedCompletion(t *testing.T) {
	for _, damage := range []string{"hash", "binding", "request", "state", "missing", "evidence", "schema"} {
		t.Run(damage, func(t *testing.T) {
			c, target, _, _ := registeredPlugin(t, "")
			call := nativePluginCall(t, c, target, "original output")
			executor := pluginTool{core: c}
			if _, err := executor.Execute(t.Context(), call); err != nil {
				t.Fatal(err)
			}
			c.CloseResources()
			// Model disk corruption after a valid commit, including records whose
			// byte digest still matches but whose binding/schema/evidence do not.
			err := c.Store.Transaction(t.Context(), func(tx *sql.Tx) error {
				var raw string
				if err := tx.QueryRowContext(t.Context(), `SELECT result FROM plugin_calls WHERE operation_id=?`, call.ID).Scan(&raw); err != nil {
					return err
				}
				var result plugin.CallResult
				if err := json.Unmarshal([]byte(raw), &result); err != nil {
					return err
				}
				switch damage {
				case "binding":
					result.InstanceID = "another-instance"
				case "request":
					result.RequestID = "another-request"
				case "state":
					result.State = "failed"
				case "missing":
					result.Completion = nil
				case "evidence":
					result.Completion.Output = json.RawMessage(`{"text":"unproven output"}`)
				case "schema":
					result.Completion.Output = json.RawMessage(`42`)
					result.Evidence, _ = json.Marshal(result.Completion)
				}
				updated, err := json.Marshal(result)
				if err != nil {
					return err
				}
				hash := fmt.Sprintf("%x", sha256.Sum256(updated))
				if damage == "hash" {
					hash = strings.Repeat("0", 64)
				}
				if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER plugin_result_immutable`); err != nil {
					return err
				}
				_, err = tx.ExecContext(t.Context(), `UPDATE plugin_calls SET result=?,result_hash=? WHERE operation_id=?`, string(updated), hash, call.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Execute(t.Context(), call); !isFailure(err, "operation_unknown") {
				t.Fatal("corrupt result became model output", err)
			}
			if _, err := executor.Recover(t.Context(), call); !isFailure(err, "operation_unknown") {
				t.Fatal("corrupt result was recovered", err)
			}
			if nativePluginAudit(t) != 1 {
				t.Fatal("corrupt result repeated effect")
			}
		})
	}
}

func TestPluginNativeLoopAndTaskStop(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			mode := ""
			if stop {
				mode = "call-cancel"
			}
			c, target, _, _ := registeredPlugin(t, mode)
			args := nativePluginInput(t, c, target, "tool", "loop result")
			script := fixture.New(fixture.Step{Events: fixture.Call("model-call", "plugin_invoke", string(args)), Check: func(r model.Request) error {
				if len(r.Tools) != 1 || r.Tools[0].Name != "plugin_invoke" || !strings.Contains(r.System, "plugin_invoke") {
					return fmt.Errorf("native descriptor missing")
				}
				return nil
			}}, fixture.Step{Events: fixture.Answer("final", "plugin done"), Check: func(r model.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if last.Role != "tool" || last.Content[0].CallID != "model-call" || !strings.Contains(last.Content[0].Text, "loop result") {
					return fmt.Errorf("native result not paired")
				}
				return nil
			}})
			c.Models = &savedFixture{adapter: script}
			var err error
			c.Tools, err = tool.NewRegistry(c.PluginExecutors()...)
			if err != nil {
				t.Fatal(err)
			}
			a := submitWork(t, c, target.AgentID, "use native plugin")
			runCore(t, c)
			if stop {
				deadline := time.Now().Add(5 * time.Second)
				for nativePluginAudit(t) == 0 {
					if time.Now().After(deadline) {
						t.Fatal("native call not dispatched")
					}
					time.Sleep(5 * time.Millisecond)
				}
				req, _ := protocol.NewRequest("agent.stop", "stop", protocol.Target{AgentID: a.AgentID})
				if _, err := c.Handle(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				waitTask(t, c, a, "cancelled")
				if len(script.Requests()) != 1 {
					t.Fatal("stopped task queried model again")
				}
			} else {
				waitTask(t, c, a, "completed")
				if len(script.Requests()) != 2 {
					t.Fatal(len(script.Requests()))
				}
			}
			if nativePluginAudit(t) != 1 {
				t.Fatal("loop repeated plugin call")
			}
		})
	}
}

func TestPluginNativeRestartRecoversResultWithoutPluginProcess(t *testing.T) {
	for _, tc := range []struct{ mode, failure string }{{}, {"call-failed", "plugin_call_failed"}, {"call-exit", "operation_unknown"}} {
		t.Run(tc.mode, func(t *testing.T) {
			failed, unknown := tc.mode == "call-failed", tc.mode == "call-exit"
			c, _, manifest, source := runtimePluginFixture(t)
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "recovered plugin"), Check: func(r model.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if r.System != "original system prompt" || last.Role != "tool" || last.Content[0].CallID != "model-call" || last.Content[0].IsError != failed {
					return fmt.Errorf("lost original context")
				}
				return nil
			}})
			var err error
			c.Tools, err = tool.NewRegistry(c.PluginExecutors()...)
			if err != nil {
				t.Fatal(err)
			}
			a, event, restorer := savedWork(t, c, script)
			c.RestoreModels = restorer
			t.Setenv("ZENDE_TEST_PLUGIN_MODE", tc.mode)
			saveRuntimePlugin(t, source, manifest)
			register, _ := protocol.NewRequest("resource.register", "register", protocol.RegisterResource{AgentID: a.AgentID, Kind: "plugin", Path: source})
			v, err := c.Handle(t.Context(), register)
			if err != nil {
				t.Fatal(err)
			}
			target := protocol.ResourceTarget{AgentID: a.AgentID, ID: v.(protocol.Accepted).Resource.ID}
			args := nativePluginInput(t, c, target, "tool", "original output")
			modelCall := model.ToolCall{ID: "model-call", Name: "plugin_invoke", Arguments: args}
			journalResponse(t, c, a, modelCall)
			op := invocation{SchemaVersion: 2, ID: "original-call", RequestID: "request-original", Call: modelCall, Version: "1"}
			event("tool.started", op)
			event("tool.dispatching", op)
			executor := pluginTool{core: c}
			if _, err := executor.Execute(t.Context(), tool.Call{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: args}); (tc.failure == "" && err != nil) || (tc.failure != "" && !isFailure(err, tc.failure)) {
				t.Fatal(err)
			}
			c.CloseResources()
			replacement := reopenCore(t, c)
			replacement.Tools, err = tool.NewRegistry(replacement.PluginExecutors()...)
			if err != nil {
				t.Fatal(err)
			}
			runCore(t, replacement)
			if unknown {
				waitTask(t, replacement, a, "unknown")
				queued := submitWork(t, replacement, a.AgentID, "continue")
				req, _ := protocol.NewRequest("agent.recover", "verify-original", protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID})
				if _, err := replacement.Handle(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				held := false
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !held; {
					page, err := replacement.Store.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range page.Events {
						if event.Kind == "recovery.held" {
							held = true
						}
					}
					time.Sleep(5 * time.Millisecond)
				}
				if !held || len(script.Requests()) != 0 || nativePluginAudit(t) != 1 {
					t.Fatal("unverified plugin resumed or replayed", held, script.Requests())
				}
				waitTask(t, replacement, queued, "accepted")
				return
			}
			snap := waitTask(t, replacement, a, "completed")
			if len(snap.Tasks) != 1 || snap.Tasks[0].Result != "recovered plugin" || nativePluginAudit(t) != 1 || restorer.restores.Load() != 1 {
				t.Fatal(snap)
			}
		})
	}
}

var _ tool.Recoverer = pluginTool{}
