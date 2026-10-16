package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

func waitPluginReceipt(t *testing.T, c *Core, target protocol.PluginCallTarget, match func(store.PluginReceipt) bool) store.PluginReceipt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := c.Store.PluginCall(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil && match(r.Receipt) {
			return r.Receipt
		}
		if time.Now().After(deadline) {
			t.Fatal("plugin did not reach expected state", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pluginTestSQL(t *testing.T, c *Core, statement string) {
	t.Helper()
	if err := c.Store.Transaction(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), statement); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestPluginExecutionDetachRetryAndRefresh(t *testing.T) {
	releaseFile := filepath.Join(t.TempDir(), "release")
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", releaseFile)
	c, target, manifest, source := registeredPlugin(t, "call-block")
	_, inv, release := prepareRuntimePlugin(t, c, target, "original", "command")
	release()
	client, detach := context.WithCancel(t.Context())
	receipt, err := c.startPluginCall(client, inv.Request, inv.Scope)
	if err != nil || receipt.State != "dispatched" {
		t.Fatal(receipt, err)
	}
	detach()
	waitPluginReceipt(t, c, receipt.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry := inv.Request
			retry.ID = protocol.NewID("transport")
			r, err := c.startPluginCall(t.Context(), retry, inv.Scope)
			if err != nil || r.Binding != receipt.Binding || r.State != "dispatched" {
				t.Error(r, err)
			}
		}()
	}
	wg.Wait()
	changed := inv.Request
	var input store.PluginCallInput
	json.Unmarshal(changed.Params, &input)
	input.Arguments = json.RawMessage(`{"text":"different effect"}`)
	changed.Params, _ = json.Marshal(input)
	if _, err := c.startPluginCall(t.Context(), changed, inv.Scope); !isFailure(err, "operation_conflict") {
		t.Fatal(err)
	}
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	_, next, free := prepareRuntimePlugin(t, c, target, "next", "command")
	free()
	r, err := c.startPluginCall(t.Context(), next.Request, next.Scope)
	if err != nil || r.Binding.InstanceID == receipt.Binding.InstanceID {
		t.Fatal(r, err)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
	if err := os.WriteFile(releaseFile, []byte("finish"), 0600); err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, receipt.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
	c.CloseResources()
	r, err = c.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil || r.State != "completed" || r.Binding != receipt.Binding {
		t.Fatal("offline retry did not return original", r, err)
	}
	audit, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
	if err != nil || bytes.Count(audit, []byte(`"method":"plugin.command.call"`)) != 2 {
		t.Fatal("replayed command", string(audit), err)
	}
}

func TestPluginExecutionCancellationCommitAndTerminalPriority(t *testing.T) {
	for _, mode := range []string{"call-cancel", "call-block"} {
		t.Run(mode, func(t *testing.T) {
			releaseFile := filepath.Join(t.TempDir(), "release")
			t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", releaseFile)
			c, target, _, _ := registeredPlugin(t, mode)
			_, inv, free := prepareRuntimePlugin(t, c, target, "original", "tool")
			free()
			r, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
			if err != nil {
				t.Fatal(err)
			}
			waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
			pluginTestSQL(t, c, `CREATE TRIGGER reject_plugin_cancel BEFORE INSERT ON operations WHEN NEW.id='cancel-denied' BEGIN SELECT RAISE(ABORT,'injected rollback'); END`)
			cancelReq, _ := protocol.NewRequest("plugin.cancel", "cancel-denied", r.PluginCallTarget)
			if _, err := c.Handle(t.Context(), cancelReq); err == nil {
				t.Fatal("accepted failed cancellation transaction")
			}
			original, err := c.Store.PluginCall(t.Context(), r.PluginCallTarget)
			if err != nil || original.Receipt.State != "dispatched" {
				t.Fatal(original, err)
			}
			pluginTestSQL(t, c, `DROP TRIGGER reject_plugin_cancel`)
			cancelReq.OperationID = "cancel"
			first, err := c.Handle(t.Context(), cancelReq)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "call-block" {
				waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "cancelling" })
				if err := os.WriteFile(releaseFile, []byte("finish"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			want := "cancelled"
			if mode == "call-block" {
				want = "completed"
			}
			waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == want })
			second, err := c.Handle(t.Context(), cancelReq)
			a, _ := json.Marshal(first)
			b, _ := json.Marshal(second)
			if err != nil || !bytes.Equal(a, b) {
				t.Fatal(first, second, err)
			}
			page, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, e := range page.Events {
				if e.Kind == "plugin.cancelling" {
					count++
				}
			}
			if count != 1 {
				t.Fatal("cancellation was lost or repeated", count)
			}
		})
	}
}

func TestPluginExecutionPersistenceFailureIsHeldAcrossRestart(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	pluginTestSQL(t, c, `CREATE TRIGGER reject_plugin_result BEFORE UPDATE OF state ON plugin_calls WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'injected result failure'); END`)
	_, inv, free := prepareRuntimePlugin(t, c, target, "original", "tool")
	free()
	r, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !isFailure(c.pluginCallFailure(r.PluginCallTarget), "persistence_failed") {
		if time.Now().After(deadline) {
			t.Fatal("persistence failure was not observable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	inspect, _ := protocol.NewRequest("plugin.inspect", "", r.PluginCallTarget)
	if _, err := c.Handle(t.Context(), inspect); !isFailure(err, "persistence_failed") {
		t.Fatal(err)
	}
	next := inv.Request
	next.OperationID = "new-id"
	if _, err := c.startPluginCall(t.Context(), next, inv.Scope); !isFailure(err, "persistence_failed") {
		t.Fatal("new ID bypassed uncommitted result", err)
	}
	c.CloseResources()
	pluginTestSQL(t, c, `DROP TRIGGER reject_plugin_result`)
	replacement := New(c.Store, c.Config)
	defer replacement.CloseResources()
	if err := c.Store.InterruptPluginCalls(t.Context(), replacement.InstanceID); err != nil {
		t.Fatal(err)
	}
	old, err := replacement.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil || old.State != "unknown" || old.Binding != r.Binding {
		t.Fatal(old, err)
	}
	// Recreate the instance explicitly; a new identity still cannot skip the
	// original unknown effect once preparation reaches the store fence.
	refresh, _ := protocol.NewRequest("resource.refresh", "restart", target)
	if _, err := replacement.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.startPluginCall(t.Context(), next, inv.Scope); !isFailure(err, "operation_unknown") {
		t.Fatal("new ID bypassed recovered unknown effect", err)
	}
}

func TestPluginExecutionTaskStopAndCompletionFence(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "call-cancel")
	input := protocol.Submit{AgentID: target.AgentID, Input: "work"}
	req, _ := protocol.NewRequest("agent.submit", "task", input)
	a, err := c.Store.Submit(t.Context(), req, input, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.ClaimNext(t.Context(), target.AgentID); err != nil {
		t.Fatal(err)
	}
	_, inv, free := prepareRuntimePlugin(t, c, target, "task-plugin", "tool")
	free()
	var args store.PluginCallInput
	json.Unmarshal(inv.Request.Params, &args)
	args.TaskID, inv.Scope.TaskID = a.TaskID, a.TaskID
	inv.Request.Params, _ = json.Marshal(args)
	r, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
	if err := c.Store.FinishTask(t.Context(), target.AgentID, a.TaskID, "completed", "too early", nil); !isFailure(err, "plugin_pending") {
		t.Fatal("Task ended while its plugin was running", err)
	}
	observe, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	if err := c.waitPluginTask(observe, target.AgentID, a.TaskID); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	cancel()
	stop, _ := protocol.NewRequest("agent.stop", "stop", protocol.Target{AgentID: target.AgentID})
	if _, err := c.Handle(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "cancelled" })
	if err := c.waitPluginTask(t.Context(), target.AgentID, a.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.FinishTask(t.Context(), target.AgentID, a.TaskID, "cancelled", "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestPluginExecutionUnknownTaskAndConcurrentShutdown(t *testing.T) {
	c, target, _, source := registeredPlugin(t, "call-exit")
	input := protocol.Submit{AgentID: target.AgentID, Input: "work"}
	req, _ := protocol.NewRequest("agent.submit", "task", input)
	a, err := c.Store.Submit(t.Context(), req, input, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.ClaimNext(t.Context(), target.AgentID); err != nil {
		t.Fatal(err)
	}
	_, inv, free := prepareRuntimePlugin(t, c, target, "task-plugin", "tool")
	free()
	var args store.PluginCallInput
	json.Unmarshal(inv.Request.Params, &args)
	args.TaskID, inv.Scope.TaskID = a.TaskID, a.TaskID
	inv.Request.Params, _ = json.Marshal(args)
	r, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "unknown" })
	if err := c.waitPluginTask(t.Context(), target.AgentID, a.TaskID); !isFailure(err, "operation_unknown") {
		t.Fatal(err)
	}
	if err := c.Store.FinishTask(t.Context(), target.AgentID, a.TaskID, "completed", "invented", nil); !isFailure(err, "operation_unknown") {
		t.Fatal(err)
	}
	if err := c.Store.FinishTask(t.Context(), target.AgentID, a.TaskID, "unknown", "", nil); err != nil {
		t.Fatal(err)
	}
	// A different Agent on the same Core can still prepare and use the plugin.
	create := protocol.CreateAgent{Cwd: filepath.Dir(source)}
	newAgent, _ := protocol.NewRequest("agent.create", "other-agent", create)
	other, err := c.Store.Create(t.Context(), newAgent, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	register, _ := protocol.NewRequest("resource.register", "other-plugin", protocol.RegisterResource{AgentID: other.AgentID, Kind: "plugin", Path: source})
	published, err := c.Handle(t.Context(), register)
	if err != nil {
		t.Fatal(err)
	}
	otherTarget := protocol.ResourceTarget{AgentID: other.AgentID, ID: published.(protocol.Accepted).Resource.ID}
	_, next, release := prepareRuntimePlugin(t, c, otherTarget, "other-call", "tool")
	release()
	nextReceipt, err := c.startPluginCall(t.Context(), next.Request, next.Scope)
	if err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, nextReceipt.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); c.CloseResources() }()
	}
	wg.Wait()
	if got, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope); err != nil || got.State != "unknown" {
		t.Fatal(got, err)
	}
}

func TestPluginExecutionShutdownReclaimsActiveWorkers(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "call-cancel")
	_, inv, free := prepareRuntimePlugin(t, c, target, "active-at-shutdown", "command")
	free()
	r, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
	if err != nil {
		t.Fatal(err)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); c.CloseResources() }()
	}
	wg.Wait()
	if len(c.pluginCalls) != 0 || len(c.pluginOwned) != 0 {
		t.Fatal("shutdown left active plugin workers or instances")
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "cancelled" })
	fresh := inv.Request
	fresh.OperationID = "after-shutdown"
	if _, err := c.startPluginCall(t.Context(), fresh, inv.Scope); !isFailure(err, "unavailable") {
		t.Fatal(err)
	}
}

func TestPluginExecutionObserverPanicRetainsOriginalIdentity(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	call, inv, release := prepareRuntimePlugin(t, c, target, "observer-panic", "tool")
	defer release()
	beginRuntimePlugin(t, c, inv)
	callTarget := protocol.PluginCallTarget{AgentID: target.AgentID, OperationID: inv.Request.OperationID}
	result := invokePluginCall(t.Context(), call, func(p plugin.Progress) error {
		if err := c.Store.AppendPluginProgress(t.Context(), callTarget, c.InstanceID, p); err != nil {
			return err
		}
		panic("private observer failure")
	})
	if result.State != "unknown" || result.CallBinding != inv.Binding || result.RequestID == "" || result.Completion != nil {
		t.Fatal(result)
	}
	if _, err := c.Store.FinishPluginCall(t.Context(), callTarget, c.InstanceID, result); err != nil {
		t.Fatal(err)
	}
}
