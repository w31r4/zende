package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

func registeredPlugin(t *testing.T, mode string) (*Core, protocol.ResourceTarget, plugin.Manifest, string) {
	t.Helper()
	c, agent, manifest, source := runtimePluginFixture(t)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	saveRuntimePlugin(t, source, manifest)
	p := protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source}
	req, _ := protocol.NewRequest("resource.register", "register", p)
	value, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := value.(protocol.Accepted)
	return c, protocol.ResourceTarget{AgentID: agent, ID: a.Resource.ID}, manifest, source
}

func prepareRuntimePlugin(t *testing.T, c *Core, target protocol.ResourceTarget, operation, kind string) (*plugin.Call, store.PluginInvocation, func()) {
	t.Helper()
	catalog, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: target.AgentID, ID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, d := range catalog.Definitions {
		if d.Kind == kind {
			ref = d.Ref
		}
	}
	agent, err := c.Store.Agent(t.Context(), target.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("plugin."+kind+".call", operation, store.PluginCallInput{AgentID: target.AgentID, ResourceID: target.ID, DefinitionRef: ref, Arguments: json.RawMessage(`{"text":"completed-on-original-instance"}`)})
	call, inv, release, err := c.preparePluginCall(req, plugin.Scope{AgentID: target.AgentID, Cwd: agent.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return call, inv, release
}

func beginRuntimePlugin(t *testing.T, c *Core, inv store.PluginInvocation) {
	t.Helper()
	if r, fresh, err := c.Store.BeginPluginCall(t.Context(), inv, c.InstanceID); err != nil || !fresh || r.Receipt.State != "dispatched" {
		t.Fatal(r, fresh, err)
	}
}

func waitPluginClosed(t *testing.T, instance *plugin.Instance) {
	t.Helper()
	select {
	case <-instance.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("plugin was not reclaimed")
	}
}

func TestPluginLeasePreservesRunningCallAcrossRefresh(t *testing.T) {
	releaseFile := filepath.Join(t.TempDir(), "finish-call")
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", releaseFile)
	c, target, manifest, source := registeredPlugin(t, "call-block")
	old := c.pluginSlots[target].instance
	call, inv, release := prepareRuntimePlugin(t, c, target, "old-call", "tool")
	beginRuntimePlugin(t, c, inv)
	callTarget := store.PluginCallTarget{AgentID: target.AgentID, OperationID: inv.Request.OperationID}
	progress := make(chan struct{}, 1)
	type outcome struct {
		result plugin.CallResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := call.Invoke(t.Context(), func(p plugin.Progress) error {
			err := c.Store.AppendPluginProgress(t.Context(), callTarget, c.InstanceID, p)
			progress <- struct{}{}
			return err
		})
		done <- outcome{result, err}
	}()
	select {
	case <-progress:
	case <-time.After(5 * time.Second):
		t.Fatal("old call never started")
	}
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "invalid")
	req, _ := protocol.NewRequest("resource.refresh", "invalid-update", target)
	if _, err := c.Handle(t.Context(), req); err == nil {
		t.Fatal("invalid candidate published")
	}
	if old.Info().State != "ready" {
		t.Fatal("failed refresh stopped original")
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	req, _ = protocol.NewRequest("resource.refresh", "valid-update", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 2 || instances.Current == old.Info().InstanceID {
		t.Fatal(instances, err)
	}
	for _, item := range instances.Items {
		if item.Instance.InstanceID == old.Info().InstanceID && (!item.Retiring || item.References != 1 || item.Instance.State != "ready") {
			t.Fatal(item)
		}
	}
	// New calls select the replacement; the old definition cannot be redirected.
	next, nextInv, nextRelease := prepareRuntimePlugin(t, c, target, "new-call", "command")
	if nextInv.Binding.InstanceID == inv.Binding.InstanceID || nextInv.ResourceVersion == inv.ResourceVersion {
		t.Fatal("new call selected old instance")
	}
	beginRuntimePlugin(t, c, nextInv)
	nextTarget := store.PluginCallTarget{AgentID: target.AgentID, OperationID: nextInv.Request.OperationID}
	result, err := next.Invoke(t.Context(), func(p plugin.Progress) error {
		return c.Store.AppendPluginProgress(t.Context(), nextTarget, c.InstanceID, p)
	})
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	if _, err := c.Store.FinishPluginCall(t.Context(), nextTarget, c.InstanceID, result); err != nil {
		t.Fatal(err)
	}
	nextRelease()
	if _, _, freed, err := c.preparePluginCall(inv.Request, inv.Scope); !isFailure(err, "stale_reference") || freed != nil {
		t.Fatal("old ref selected replacement", err)
	}
	if err := os.WriteFile(releaseFile, []byte("finish"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-done:
		result, err = completed.result, completed.err
	case <-time.After(5 * time.Second):
		t.Fatal("old call did not finish")
	}
	if err != nil || result.State != "completed" || result.InstanceID != inv.Binding.InstanceID {
		t.Fatal(result, err)
	}
	if _, err := c.Store.FinishPluginCall(t.Context(), callTarget, c.InstanceID, result); err != nil {
		t.Fatal(err)
	}
	if old.Info().State != "ready" {
		t.Fatal("old instance closed before owner released result")
	}
	release()
	release()
	waitPluginClosed(t, old)
	deadline := time.Now().Add(5 * time.Second)
	for {
		instances, err = c.pluginInstances(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		if len(instances.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed instance retained in host", instances)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if instances.Items[0].References != 0 || instances.Items[0].Retiring {
		t.Fatal(instances)
	}
}

func TestPluginLeasePreparationAndShutdown(t *testing.T) {
	c, target, manifest, source := registeredPlugin(t, "")
	old := c.pluginSlots[target].instance
	_, inv, release := prepareRuntimePlugin(t, c, target, "prepared", "command")
	bad := inv.Request
	bad.Method = "plugin.tool.call"
	if _, _, freed, err := c.preparePluginCall(bad, inv.Scope); !isFailure(err, "invalid_params") || freed != nil {
		t.Fatal(err)
	}
	scope := inv.Scope
	scope.AgentID = "other"
	if _, _, _, err := c.preparePluginCall(inv.Request, scope); !isFailure(err, "invalid_params") {
		t.Fatal(err)
	}
	before, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(before.Items) != 1 || before.Items[0].References != 1 {
		t.Fatal("failed preparation leaked a reference", before, err)
	}
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	req, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	current := c.pluginSlots[target].instance
	_, _, currentRelease := prepareRuntimePlugin(t, c, target, "new-prepared", "tool")
	// Core shutdown owns both versions even when a call owner has not released.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); c.CloseResources() }()
	}
	wg.Wait()
	waitPluginClosed(t, old)
	waitPluginClosed(t, current)
	release()
	currentRelease()
	if _, _, err := c.leasePlugin(target); !isFailure(err, "unavailable") {
		t.Fatal(err)
	}
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 0 || instances.Current != "" {
		t.Fatal(instances, err)
	}
	if _, err := c.pluginInstances(t.Context(), protocol.ResourceTarget{AgentID: "other", ID: target.ID}); err == nil {
		t.Fatal("read another Agent's plugin")
	}
}

func TestPluginRetirementLimitPreservesCurrentAndReleasesCapacity(t *testing.T) {
	c, target, manifest, source := registeredPlugin(t, "")
	var releases []func()
	var instances []*plugin.Instance
	for n := range MaxRetiringPluginInstances {
		slot, release, err := c.leasePlugin(target)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		releases = append(releases, release)
		instances = append(instances, slot.instance)
		manifest.Version = fmt.Sprint(n + 2)
		saveRuntimePlugin(t, source, manifest)
		req, _ := protocol.NewRequest("resource.refresh", fmt.Sprint("refresh-", n), target)
		if _, err := c.Handle(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	current := c.pluginSlots[target]
	// A no-op refresh does not consume retirement capacity.
	req, _ := protocol.NewRequest("resource.refresh", "same-version-at-limit", target)
	if _, err := c.Handle(t.Context(), req); err != nil || c.pluginSlots[target] != current {
		t.Fatal("unchanged refresh replaced instance or hit retirement limit", err)
	}
	manifest.Version = "over-limit"
	saveRuntimePlugin(t, source, manifest)
	req, _ = protocol.NewRequest("resource.refresh", "over-limit", target)
	if _, err := c.Handle(t.Context(), req); !isFailure(err, "resource_limit") {
		t.Fatal(err)
	}
	if c.pluginSlots[target] != current || current.instance.Info().State != "ready" {
		t.Fatal("limit changed current instance")
	}
	status, err := c.resourceStatus(t.Context(), target)
	if err != nil || status.Version != current.version || status.Update == nil || status.Update.State != "failed" {
		t.Fatal(status, err)
	}
	page, err := c.pluginInstances(t.Context(), target)
	if err != nil || page.MaxRetiring != MaxRetiringPluginInstances || len(page.Items) != MaxRetiringPluginInstances+1 {
		t.Fatal(page, err)
	}
	releases[0]()
	waitPluginClosed(t, instances[0])
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err = c.pluginInstances(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == MaxRetiringPluginInstances {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retirement capacity not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	req, _ = protocol.NewRequest("resource.refresh", "after-release", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}
