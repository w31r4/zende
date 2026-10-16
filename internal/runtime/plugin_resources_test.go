package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func registeredResourcePlugin(t *testing.T, mode string) (*Core, protocol.ResourceTarget, plugin.Manifest, string) {
	t.Helper()
	c, agent, manifest, source := runtimePluginFixture(t)
	manifest.Capabilities = append(manifest.Capabilities, "resources")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	saveRuntimePlugin(t, source, manifest)
	r, _ := protocol.NewRequest("resource.register", "parent", protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source})
	a, err := c.Handle(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return c, protocol.ResourceTarget{AgentID: agent, ID: a.(protocol.Accepted).Resource.ID}, manifest, source
}

func registerPluginChild(t *testing.T, c *Core, parent protocol.ResourceTarget, kind string) (protocol.Request, protocol.Accepted) {
	t.Helper()
	p := protocol.RegisterResource{AgentID: parent.AgentID, Kind: kind, Path: "plugin://" + parent.ID + "/" + kind + "/fixture-" + kind}
	r, _ := protocol.NewRequest("resource.register", "child-"+kind, p)
	a, err := c.Handle(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return r, a.(protocol.Accepted)
}

func TestPluginResourcesPublishRefreshAndReadOffline(t *testing.T) {
	c, parent, m, source := registeredResourcePlugin(t, "")
	// Discovery publishes only metadata; the user/model chooses resources to load.
	var saved []protocol.Accepted
	var requests []protocol.Request
	for _, kind := range []string{"skill", "prompt"} {
		r, a := registerPluginChild(t, c, parent, kind)
		requests, saved = append(requests, r), append(saved, a)
		if a.Resource.PluginOrigin == nil || a.Resource.PluginOrigin.Plugin.ID != parent.ID || !strings.HasPrefix(a.Resource.Source, "plugin://") {
			t.Fatal(a)
		}
	}
	// Installing resources doesn't apply their bodies to model history.
	messages, _, err := c.Store.ModelContext(t.Context(), parent.AgentID)
	if err != nil || len(messages) != 0 {
		t.Fatal(messages, err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "resource-updated")
	m.Version = "2"
	saveRuntimePlugin(t, source, m)
	r, _ := protocol.NewRequest("resource.refresh", "parent-v2", parent)
	if _, err := c.Handle(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	var updated []protocol.Accepted
	for _, old := range saved {
		r, _ := protocol.NewRequest("resource.refresh", "refresh-"+old.Resource.Kind, protocol.ResourceTarget{AgentID: parent.AgentID, ID: old.Resource.ID})
		a, err := c.Handle(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		got := a.(protocol.Accepted)
		if got.Resource.ID != old.Resource.ID || got.Resource.Version == old.Resource.Version || got.Resource.PluginOrigin.Plugin.Version == old.Resource.PluginOrigin.Plugin.Version {
			t.Fatal(got)
		}
		updated = append(updated, got)
	}
	// A broken new read leaves the last complete child version ready.
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "resource-corrupt")
	m.Version = "3"
	saveRuntimePlugin(t, source, m)
	r, _ = protocol.NewRequest("resource.refresh", "parent-v3", parent)
	if _, err := c.Handle(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, current := range updated {
		target := protocol.ResourceTarget{AgentID: parent.AgentID, ID: current.Resource.ID}
		r, _ := protocol.NewRequest("resource.refresh", "bad-"+current.Resource.Kind, target)
		if _, err := c.Handle(t.Context(), r); !isFailure(err, "invalid_resource") {
			t.Fatal(err)
		}
		status, err := c.resourceStatus(t.Context(), target)
		if err != nil || status.State != "ready" || status.Update.State != "failed" || status.Version != current.Resource.Version {
			t.Fatal(status, err)
		}
	}
	c.CloseResources()
	for n, old := range saved {
		// Idempotent retry after shutdown is a receipt lookup, not a plugin read.
		got, err := c.Handle(t.Context(), requests[n])
		if err != nil || !reflect.DeepEqual(got, old) {
			t.Fatal(got, err)
		}
		for _, a := range []protocol.Accepted{old, updated[n]} {
			content, err := c.Store.ReadResource(t.Context(), protocol.ResourceRead{AgentID: parent.AgentID, ID: a.Resource.ID, Version: a.Resource.Version})
			if err != nil || !reflect.DeepEqual(content.PluginOrigin, a.Resource.PluginOrigin) || content.Text == "" {
				t.Fatal(content, err)
			}
		}
	}
	// User-selected plugin skills use the same pinned input path as local skills.
	in := protocol.Submit{AgentID: parent.AgentID, Input: "use original skill", Skills: []protocol.ResourceRef{{ID: saved[0].Resource.ID, Version: saved[0].Resource.Version}}}
	r, _ = protocol.NewRequest("agent.submit", "selected", in)
	if _, err := c.Store.Submit(t.Context(), r, in, 8); err != nil {
		t.Fatal(err)
	}
	if work, err := c.Store.ClaimNext(t.Context(), parent.AgentID); err != nil || work == nil {
		t.Fatal(work, err)
	}
	messages, _, err = c.Store.ModelContext(t.Context(), parent.AgentID)
	raw, _ := json.Marshal(messages)
	if err != nil || !strings.Contains(string(raw), "Read this fixture") || strings.Contains(string(raw), "Updated fixture") || !strings.Contains(string(raw), saved[0].Resource.Source) {
		t.Fatal(string(raw), err)
	}
}

func TestPluginResourceParentRefreshDuringReadRejectsMixedVersion(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", release)
	c, parent, m, source := registeredResourcePlugin(t, "resource-block")
	old := c.pluginSlots[parent].instance
	p := protocol.RegisterResource{AgentID: parent.AgentID, Kind: "skill", Path: "plugin://" + parent.ID + "/skill/fixture-skill"}
	r, _ := protocol.NewRequest("resource.register", "slow-child", p)
	done := make(chan error, 1)
	go func() { _, err := c.Handle(t.Context(), r); done <- err }()
	waitPluginResourceRead(t, filepath.Join(filepath.Dir(source), "audit.jsonl"))
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "resource-updated")
	m.Version = "2"
	saveRuntimePlugin(t, source, m)
	refresh, _ := protocol.NewRequest("resource.refresh", "new-parent", parent)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	if old.Info().State != "ready" {
		t.Fatal("read lost old lease", old.Info())
	}
	if err := os.WriteFile(release, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !isFailure(err, "resource_changed") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not finish")
	}
	waitPluginClosed(t, old)
	// A failed request is retryable, and now resolves the new complete package.
	a, err := c.Handle(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	got := a.(protocol.Accepted)
	if got.Resource.PluginOrigin.Plugin.Version != c.pluginSlots[parent].version {
		t.Fatal(got)
	}
}

func waitPluginResourceRead(t *testing.T, audit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		raw, _ := os.ReadFile(audit)
		if strings.Contains(string(raw), "plugin.resource.read") {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("resource read did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
