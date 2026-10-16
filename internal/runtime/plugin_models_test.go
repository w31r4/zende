package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func registeredModelPlugin(t *testing.T, mode string) (*Core, protocol.ResourceTarget, plugin.Manifest, string, string) {
	t.Helper()
	c, agent, manifest, source := runtimePluginFixture(t)
	manifest.Capabilities = append(manifest.Capabilities, "providers")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	saveRuntimePlugin(t, source, manifest)
	req, _ := protocol.NewRequest("resource.register", "register-model", protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source})
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := v.(protocol.Accepted)
	return c, protocol.ResourceTarget{AgentID: agent, ID: a.Resource.ID}, manifest, source, "plugin:" + a.Resource.ID + ":fixture-models/fixture-text"
}

func TestPluginModelRunsTwoToolRoundsWithoutConfiguredProvider(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "model-tools")
	var calls atomic.Int32
	var err error
	c.Tools, err = tool.NewRegistry(
		testTool{"echo", func(_ context.Context, call tool.Call) (tool.Result, error) {
			calls.Add(1)
			return tool.Result{Content: []model.Block{{Type: "text", Text: "echoed"}}}, nil
		}},
		testTool{"fail", func(_ context.Context, call tool.Call) (tool.Result, error) {
			calls.Add(1)
			return tool.Result{}, protocol.E("test_failure", "expected tool failure")
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	effort := "high"
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection, Effort: &effort})
	if c.Models != nil || c.RestoreModels != nil || !c.modelAvailable() {
		t.Fatal("fixture unexpectedly uses configured provider")
	}
	a := submitWork(t, c, target.AgentID, "two calls")
	runCore(t, c)
	snap := waitTask(t, c, a, "completed")
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	var final struct {
		Scope       plugin.Scope `json:"scope"`
		Version     string       `json:"version"`
		Effort      string       `json:"effort"`
		ToolResults int          `json:"tool_results"`
	}
	for _, task := range snap.Tasks {
		if task.ID == a.TaskID {
			if err := json.Unmarshal([]byte(task.Result), &final); err != nil {
				t.Fatal(err)
			}
		}
	}
	if final.Scope.AgentID != target.AgentID || final.Scope.TaskID != a.TaskID || final.Scope.Cwd != c.Config.Config.Cwd || final.Version != "1" || final.Effort != effort || final.ToolResults != 2 {
		t.Fatal(final)
	}
	events, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	for _, e := range events.Events {
		if e.Kind != "model.started" {
			continue
		}
		var r requestRecord
		if protocol.StrictDecode(e.Payload, &r) != nil || r.PluginBinding == nil || r.PluginBinding.OperationID != r.ID || r.PluginBinding.InstanceID == "" || r.PluginBinding.Resource.ID != target.ID || r.PluginBinding.Generation == 0 || r.Settings == nil || !strings.HasPrefix(r.Protocol, "plugin-model/") {
			t.Fatal("missing durable instance binding", string(e.Payload))
		}
		requests++
	}
	if requests != 3 {
		t.Fatal(requests)
	}
	// A second Task restores the exact selected snapshot, not the profile
	// default or a freshly guessed provider definition.
	next := submitWork(t, c, target.AgentID, "continue")
	waitTask(t, c, next, "completed")
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 1 || instances.Items[0].References != 0 {
		t.Fatal("model lease leaked", instances, err)
	}
}

func TestPluginModelLeaseRefreshAndExactRestore(t *testing.T) {
	releaseFile := filepath.Join(t.TempDir(), "release-model")
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", releaseFile)
	c, target, manifest, source, selection := registeredModelPlugin(t, "model-block")
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	choice, err := c.Store.Selection(t.Context(), target.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := c.restoreBinding(t.Context(), target.AgentID, choice.Binding)
	if err != nil {
		t.Fatal(err)
	}
	scope := plugin.Scope{AgentID: target.AgentID, TaskID: "task", Cwd: c.Config.Config.Cwd}
	adapter, actual, release, err := c.prepareModelRequest(binding, scope, "old-request")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	old := c.pluginSlots[target].instance
	started, done := make(chan struct{}), make(chan error, 1)
	var collector model.Collector
	go func() {
		done <- adapter.Stream(t.Context(), model.Request{ID: "old-request", Model: binding.Model, MaxOutputTokens: binding.MaxOutputTokens}, func(d model.Delta) error {
			if d.Kind == model.MessageStart {
				close(started)
			}
			return collector.Add(d)
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old model stream not started")
	}
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "invalid-provider")
	req, _ := protocol.NewRequest("resource.refresh", "bad-model-update", target)
	if _, err := c.Handle(t.Context(), req); err == nil {
		t.Fatal("invalid provider published")
	}
	if old.Info().State != "ready" {
		t.Fatal("failed candidate stopped old stream")
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	req, _ = protocol.NewRequest("resource.refresh", "good-model-update", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 2 {
		t.Fatal(instances, err)
	}
	for _, item := range instances.Items {
		if item.Instance.InstanceID == actual.InstanceID && (!item.Retiring || item.References != 1) {
			t.Fatal(item)
		}
	}
	if oldBinding, err := c.restoreBinding(t.Context(), target.AgentID, choice.Binding); err != nil || !reflect.DeepEqual(oldBinding.Snapshot, binding.Snapshot) {
		t.Fatal("old snapshot could not be reconstructed independently of process readiness", err)
	}
	if _, _, free, err := c.prepareModelRequest(binding, scope, "after-refresh"); !isFailure(err, "stale_reference") || free != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old stream interrupted")
	}
	response, err := collector.Response()
	if err != nil {
		t.Fatal(err)
	}
	text, final, err := response.Final()
	if err != nil || !final || !strings.Contains(text, `"version":"1"`) {
		t.Fatal(text, err)
	}
	release()
	waitPluginClosed(t, old)
	current, err := c.Store.Selection(t.Context(), target.AgentID)
	if err != nil || !reflect.DeepEqual(choice, current) {
		t.Fatal("refresh changed selected model", err)
	}
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	next, _ := c.Store.Selection(t.Context(), target.AgentID)
	if reflect.DeepEqual(next.Binding, choice.Binding) {
		t.Fatal("explicit selection kept old version")
	}
	newBinding, err := c.restoreBinding(t.Context(), target.AgentID, next.Binding)
	if err != nil {
		t.Fatal(err)
	}
	if newBinding.Protocol == binding.Protocol {
		t.Fatal("private namespaces crossed versions")
	}
}

func TestPluginModelRestoreRequiresOwnedVersionAndDispatchRequiresReady(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "")
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	choice, _ := c.Store.Selection(t.Context(), target.AgentID)
	b, err := c.restoreBinding(t.Context(), target.AgentID, choice.Binding)
	if err != nil {
		t.Fatal(err)
	}
	other := createWork(t, c, "")
	if _, err := c.restoreBinding(t.Context(), other.AgentID, choice.Binding); err == nil {
		t.Fatal("another Agent borrowed plugin binding")
	}
	oldInstance := c.pluginSlots[target].instance.Info().InstanceID
	c.CloseResources()
	replacement := New(c.Store, c.Config)
	t.Cleanup(replacement.CloseResources)
	offline, err := replacement.restoreBinding(t.Context(), target.AgentID, choice.Binding)
	if err != nil || !reflect.DeepEqual(offline.Snapshot, b.Snapshot) {
		t.Fatal("offline snapshot unavailable", err)
	}
	if _, _, free, err := replacement.prepareModelRequest(offline, plugin.Scope{AgentID: target.AgentID}, "offline"); !isFailure(err, "unavailable") || free != nil {
		t.Fatal("offline model was dispatched", err)
	}
	req, _ := protocol.NewRequest("resource.refresh", "restore-model-process", target)
	if _, err := replacement.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	loaded, err := replacement.restoreBinding(t.Context(), target.AgentID, choice.Binding)
	if err != nil || !reflect.DeepEqual(loaded.Snapshot, b.Snapshot) || loaded.Protocol != b.Protocol || replacement.pluginSlots[target].instance.Info().InstanceID == oldInstance {
		t.Fatal("snapshot changed on same-version restart", loaded, err)
	}
	s := &taskSettings{Version: 2, Selection: selection, Cwd: c.Config.Config.Cwd, Model: b.Model, Protocol: b.Protocol, Binding: b.Snapshot, MaxOutputTokens: b.MaxOutputTokens, NativeCategories: b.NativeCategories}
	resume, err := replacement.prepareResume(t.Context(), &recoveryJournal{Work: store.Work{Agent: protocol.Agent{ID: target.AgentID}}, Settings: s})
	if err != nil || resume == nil || !reflect.DeepEqual(resume.Binding.Snapshot, b.Snapshot) {
		t.Fatal(resume, err)
	}
}

func TestPluginModelPartialCallDoesNotExecute(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "model-partial")
	var calls atomic.Int32
	c.Tools, _ = tool.NewRegistry(testTool{"echo", func(context.Context, tool.Call) (tool.Result, error) { calls.Add(1); return tool.Result{}, nil }})
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	a := submitWork(t, c, target.AgentID, "broken tool stream")
	runCore(t, c)
	// Wait past the scheduler's automatic recovery attempt. Merely observing
	// the first unknown state misses a regression that later marks it failed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		held := false
		for _, event := range page.Events {
			if event.TaskID == a.TaskID && event.Kind == "task.recovered" {
				t.Fatal("offline plugin task was activated", string(event.Payload))
			}
			held = held || event.TaskID == a.TaskID && event.Kind == "recovery.held"
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline dependency did not retain the Task", page)
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitTask(t, c, a, "unknown")
	if calls.Load() != 0 {
		t.Fatal("partial model tool call executed")
	}
}

func TestPluginModelUnavailableBeforeDispatchKeepsTask(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "")
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	a := submitWork(t, c, target.AgentID, "resume when this model is ready")
	w, err := c.Store.ClaimNext(t.Context(), a.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	// Simulate loss of readiness after scheduling, before dispatch.
	c.pluginSlots[target].instance.Close()
	if err := c.executeWork(t.Context(), t.Context(), *w, nil); err != nil {
		t.Fatal(err)
	}
	waitTask(t, c, a, "unknown")
	j, err := readRecoveryJournal(t.Context(), c.Store, a.AgentID, a.TaskID)
	if err != nil || j.Request != nil || j.Settings == nil {
		t.Fatal(j, err)
	}
	resume, err := c.prepareResume(t.Context(), j)
	if err != nil || !isFailure(c.checkResumeModelDependency(t.Context(), resume), "unavailable") {
		t.Fatal("offline metadata must restore but cannot activate", err)
	}
	refresh, _ := protocol.NewRequest("resource.refresh", "restore-lost-model", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	runCore(t, c)
	waitTask(t, c, a, "completed")
	page, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, event := range page.Events {
		if event.TaskID == a.TaskID && event.Kind == "model.started" {
			started++
		}
	}
	if started != 1 {
		t.Fatal("unavailable model was journaled as dispatched", started)
	}
}

func TestPluginModelTaskCancellationKeepsPluginReusable(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "model-cancel")
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	a := submitWork(t, c, target.AgentID, "wait for cancellation")
	runCore(t, c)
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		started := false
		for _, e := range page.Events {
			started = started || e.TaskID == a.TaskID && e.Kind == "model.delta"
		}
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("model never started")
		}
		time.Sleep(time.Millisecond)
	}
	req, _ := protocol.NewRequest("agent.stop", "cancel-model", protocol.Target{AgentID: target.AgentID})
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	waitTask(t, c, a, "cancelled")
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 1 || instances.Items[0].References != 0 || instances.Items[0].Instance.State != "ready" {
		t.Fatal("cancelled model poisoned shared plugin", instances, err)
	}
}

func TestPluginModelJournalRejectsMismatchedInstanceBinding(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "")
	b, err := c.resolveModelSelection(t.Context(), target.AgentID, selection, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, binding, release, err := c.prepareModelRequest(b, plugin.Scope{AgentID: target.AgentID}, "request")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r := requestRecord{ID: "request", Model: b.Model, Protocol: b.Protocol, PluginBinding: binding,
		Settings:        &taskSettings{Selection: selection, Binding: b.Snapshot},
		ResourceCatalog: &protocol.ResourceCatalog{Generation: binding.Generation, Entries: []protocol.ResourceRef{binding.Resource}}}
	if !validPluginModelRecord(r) {
		t.Fatal("valid record rejected")
	}
	for _, mutate := range []func(*requestRecord){
		func(r *requestRecord) { r.PluginBinding = nil },
		func(r *requestRecord) { r.PluginBinding.OperationID = "other" },
		func(r *requestRecord) { r.PluginBinding.Resource.Version = "wrong" },
		func(r *requestRecord) { r.PluginBinding.DefinitionRef = "wrong" },
		func(r *requestRecord) { r.PluginBinding.InstanceID = "" },
		func(r *requestRecord) { r.ResourceCatalog.Entries = nil },
		func(r *requestRecord) { r.ResourceCatalog.Generation-- },
	} {
		raw, _ := json.Marshal(r)
		var changed requestRecord
		_ = json.Unmarshal(raw, &changed)
		mutate(&changed)
		if validPluginModelRecord(changed) {
			t.Fatal("mismatched binding accepted", changed)
		}
	}
}
