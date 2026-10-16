package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/tool"
)

func TestPluginModelRecoveryUsesExplicitUpgradeAndKeepsTask(t *testing.T) {
	ctx := t.Context()
	c, target, manifest, source, selection := registeredModelPlugin(t, "model-partial")
	var calls atomic.Int32
	var err error
	c.Tools, err = tool.NewRegistry(testTool{"echo", func(context.Context, tool.Call) (tool.Result, error) {
		calls.Add(1)
		return tool.Result{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	effort := "low"
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection, Effort: &effort})
	originalChoice, err := c.Store.Selection(ctx, target.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	a := submitWork(t, c, target.AgentID, "keep this task after the provider upgrade")
	w, err := c.Store.ClaimNext(ctx, target.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	oldInstance := c.pluginSlots[target].instance
	// The real plugin exits midway through tool arguments. Run the worker
	// directly so no automatic recovery races the upgrade or explicit choice.
	if err := c.executeWork(ctx, ctx, *w, nil); err != nil {
		t.Fatal(err)
	}
	waitTask(t, c, a, "unknown")
	waitPluginClosed(t, oldInstance)
	original, err := readRecoveryJournal(ctx, c.Store, a.AgentID, a.TaskID)
	if err != nil || original.Request == nil || original.Settings == nil || original.Request.PluginBinding == nil || original.ResponseComplete || len(original.Calls) != 0 {
		t.Fatal("interruption did not retain an incomplete plugin request", original, err)
	}
	if calls.Load() != 0 || !reflect.DeepEqual(original.Settings.Binding, originalChoice.Binding) {
		t.Fatal("partial call ran or original model binding was lost")
	}
	before, err := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil || before.Next != before.Latest {
		t.Fatal("incomplete baseline history", err)
	}

	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	refresh, err := protocol.NewRequest("resource.refresh", "upgrade-model-for-recovery", target)
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.Handle(ctx, refresh)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := value.(protocol.Accepted)
	if upgraded.Resource == nil || upgraded.Resource.Version == original.Request.PluginBinding.Resource.Version {
		t.Fatal("fixture did not publish a new version", upgraded)
	}
	oldBinding, err := c.restoreBinding(ctx, target.AgentID, originalChoice.Binding)
	if err != nil || !reflect.DeepEqual(oldBinding.Snapshot, originalChoice.Binding) || oldBinding.Protocol != original.Settings.Protocol {
		t.Fatal("retired binding must restore from saved metadata", oldBinding, err)
	}
	scope := plugin.Scope{AgentID: a.AgentID, TaskID: a.TaskID, Cwd: w.Agent.Cwd}
	adapter, actual, release, err := c.prepareModelRequest(oldBinding, scope, "must-not-dispatch-v1")
	if release != nil {
		release()
	}
	if !isFailure(err, "stale_reference") || adapter != nil || actual != nil || release != nil {
		t.Fatal("offline restore silently redirected old dispatch to v2", actual, err)
	}
	instances, err := c.pluginInstances(ctx, target)
	if err != nil || len(instances.Items) != 1 || instances.Items[0].References != 0 {
		t.Fatal("rejected old dispatch leaked an instance lease", instances, err)
	}
	effort = "high"
	selected := choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection, Effort: &effort})
	currentChoice, err := c.Store.Selection(ctx, target.AgentID)
	if err != nil || selected.Selection == nil || reflect.DeepEqual(currentChoice.Binding, originalChoice.Binding) {
		t.Fatal("explicit upgrade was not saved", currentChoice, err)
	}

	// Reopen the actual database with no plugin processes. Recovery must verify
	// v1's immutable settings without requiring a live v1 (or live v2) adapter.
	c.CloseResources()
	c = reopenCore(t, c)
	t.Cleanup(c.CloseResources)
	j, err := readRecoveryJournal(ctx, c.Store, a.AgentID, a.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := c.prepareResume(ctx, j)
	if err != nil || resume == nil || !reflect.DeepEqual(resume.Binding.Snapshot, originalChoice.Binding) || resume.Binding.Protocol != oldBinding.Protocol {
		t.Fatal("new explicit selection is blocked by the offline old provider", resume, err)
	}
	if !reflect.DeepEqual(j.Settings, original.Settings) || !reflect.DeepEqual(j.Request, original.Request) {
		t.Fatal("restoring the old request rewrote its settings or instance identity")
	}
	instances, err = c.pluginInstances(ctx, target)
	if err != nil || len(instances.Items) != 0 {
		t.Fatal("metadata restoration started a plugin", instances, err)
	}
	refresh, err = protocol.NewRequest("resource.refresh", "restart-upgraded-model", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Handle(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	recovery, err := protocol.NewRequest("agent.recover", "recover-original-plugin-task", protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		value, err := c.Handle(ctx, recovery)
		if err != nil {
			t.Fatal(err)
		}
		accepted := value.(protocol.Accepted)
		if accepted.AgentID != a.AgentID || accepted.TaskID != a.TaskID {
			t.Fatal("recovery created a replacement task", accepted)
		}
	}
	runCore(t, c)
	completed := waitTask(t, c, a, "completed")
	if len(completed.Tasks) != 1 || completed.Tasks[0].ID != a.TaskID || completed.Tasks[0].Input != w.Task.Input || calls.Load() != 0 {
		t.Fatal("recovery replaced work or executed the interrupted call", completed, calls.Load())
	}
	var final struct {
		Scope       plugin.Scope `json:"scope"`
		Version     string       `json:"version"`
		Effort      string       `json:"effort"`
		ToolResults int          `json:"tool_results"`
	}
	if err := json.Unmarshal([]byte(completed.Tasks[0].Result), &final); err != nil || final.Scope != scope || final.Version != "2" || final.Effort != "high" || final.ToolResults != 0 {
		t.Fatal("resumed request did not use the explicit upgraded selection", final, err)
	}
	after, err := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil || after.Next != after.Latest || len(after.Events) < len(before.Events) || !reflect.DeepEqual(after.Events[:len(before.Events)], before.Events) {
		t.Fatal("recovery changed the original history", err)
	}
	var requests []requestRecord
	counts := map[string]int{}
	for _, e := range after.Events {
		if e.TaskID != a.TaskID {
			continue
		}
		counts[e.Kind]++
		if e.Kind == "model.started" {
			var r requestRecord
			if err := protocol.StrictDecode(e.Payload, &r); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, r)
		}
	}
	if len(requests) != 2 || !reflect.DeepEqual(requests[0], *original.Request) || requests[1].ID == requests[0].ID || requests[1].PluginBinding == nil || requests[1].Settings == nil {
		t.Fatal("recovery reused a request or lost its original binding", requests)
	}
	next := requests[1]
	if next.PluginBinding.Resource.Version != upgraded.Resource.Version || next.PluginBinding.InstanceID == original.Request.PluginBinding.InstanceID || !reflect.DeepEqual(next.Settings.Binding, currentChoice.Binding) || next.Settings.ModelRevision != selected.Selection.ModelRevision || next.Settings.Effort != "high" {
		t.Fatal("resumed request did not journal the actual new binding", next)
	}
	for _, kind := range []string{"task.config", "prompt.loaded", "recovery.requested", "task.recovered", "model.interrupted", "model.completed"} {
		if counts[kind] != 1 {
			t.Fatal("recovery duplicated or omitted lifecycle records", kind, counts)
		}
	}
	if counts["tool.dispatching"] != 0 || counts["tool.completed"] != 0 || counts["model.repair"] != 0 {
		t.Fatal("recovery replayed the partial tool or consumed a format repair", counts)
	}
	instances, err = c.pluginInstances(ctx, target)
	if err != nil || len(instances.Items) != 1 || instances.Items[0].References != 0 || instances.Items[0].Instance.InstanceID != next.PluginBinding.InstanceID {
		t.Fatal("resumed model lease was not released", instances, err)
	}
}
