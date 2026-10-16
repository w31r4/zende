package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/resource/plugin/fixture"
	"zende.local/harness/internal/store"
)

func TestRuntimePluginProcessHelper(t *testing.T) { fixture.RunFromEnv() }

func TestCoreStartupHoldsUnfinishedPluginCall(t *testing.T) {
	c, agent, manifest, source := runtimePluginFixture(t)
	saveRuntimePlugin(t, source, manifest)
	p := protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source}
	req, _ := protocol.NewRequest("resource.register", "register", p)
	value, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := value.(protocol.Accepted)
	instance := c.pluginSlots[protocol.ResourceTarget{AgentID: agent, ID: a.Resource.ID}].instance
	catalog, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: agent, ID: a.Resource.ID})
	if err != nil {
		t.Fatal(err)
	}
	d := catalog.Definitions[0]
	args := json.RawMessage(`{"text":"never dispatched"}`)
	call, err := instance.PrepareCall(d.Ref, "interrupted", plugin.Scope{AgentID: agent, Cwd: filepath.Dir(source)}, args)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = protocol.NewRequest("plugin."+d.Kind+".call", "interrupted", store.PluginCallInput{AgentID: agent, ResourceID: a.Resource.ID, DefinitionRef: d.Ref, Arguments: args})
	inv := store.PluginInvocation{Request: req, Binding: call.Binding(), Scope: call.Scope(), Definition: d, ResourceVersion: a.Resource.Version, Generation: a.ResourceGeneration}
	if _, fresh, err := c.Store.BeginPluginCall(t.Context(), inv, "previous-core"); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for {
		r, err := c.Store.PriorPluginCall(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if r.Receipt.State == "unknown" {
			if r.Receipt.ResultBytes != 0 || r.Receipt.Error.Data.Kind != "operation_unknown" {
				t.Fatal("startup invented a result", r)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("startup did not hold original plugin call")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func runtimePluginFixture(t *testing.T) (*Core, string, plugin.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	t.Cleanup(c.CloseResources)
	create := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", "create", create)
	a, err := s.Create(t.Context(), req, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN", "1")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_AUDIT", filepath.Join(dir, "audit.jsonl"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := plugin.Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", Command: exe, Args: []string{"-test.run=^TestRuntimePluginProcessHelper$"}, ProtocolVersions: []int{1}, Capabilities: []string{"tools", "commands"}}
	return c, a.AgentID, m, filepath.Join(dir, "plugin.json")
}

func saveRuntimePlugin(t *testing.T, source string, m plugin.Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginPublicationHistoryAndCoreLifetime(t *testing.T) {
	c, agent, manifest, source := runtimePluginFixture(t)
	saveRuntimePlugin(t, source, manifest)
	p := protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: filepath.Base(source)}
	req, _ := protocol.NewRequest("resource.register", "register", p)
	ctx, cancel := context.WithCancel(t.Context())
	value, err := c.Handle(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	accepted := value.(protocol.Accepted)
	if accepted.Resource == nil || accepted.Resource.Kind != "plugin" {
		t.Fatal(value)
	}
	target := protocol.ResourceTarget{AgentID: agent, ID: accepted.Resource.ID}
	old := c.pluginSlots[target].instance
	if old == nil || old.Info().State != "ready" {
		t.Fatal("client cancellation stopped published process")
	}
	page, err := c.Store.Resources(t.Context(), protocol.ResourceList{AgentID: agent, Kind: "plugin"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Version != accepted.Resource.Version {
		t.Fatal(page, err)
	}
	query := protocol.PluginCatalogQuery{AgentID: agent, ID: target.ID, Version: accepted.Resource.Version}
	read, _ := protocol.NewRequest("plugin.catalog", "", query)
	value, err = c.Handle(t.Context(), read)
	if err != nil {
		t.Fatal(err)
	}
	original := value.(resource.PluginCatalog)
	if original.ManifestVersion != manifest.Digest() || len(original.Definitions) != 2 {
		t.Fatal(original)
	}
	// The same receipt is returned even when the source becomes unreadable.
	if err := os.WriteFile(source, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	value, err = c.Handle(t.Context(), req)
	if err != nil || !reflect.DeepEqual(value, accepted) || c.pluginSlots[target].instance != old {
		t.Fatal(value, err)
	}
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "invalid")
	refresh, _ := protocol.NewRequest("resource.refresh", "invalid-refresh", target)
	if _, err := c.Handle(t.Context(), refresh); err == nil {
		t.Fatal("invalid catalog accepted")
	}
	status, err := c.resourceStatus(t.Context(), target)
	if err != nil || status.State != "ready" || status.Update == nil || status.Update.State != "failed" || status.Update.Error == nil || status.Update.Error.Data.Kind != "plugin_protocol" || status.Version != accepted.Resource.Version || c.pluginSlots[target].instance != old {
		t.Fatal(status, err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	refresh, _ = protocol.NewRequest("resource.refresh", "valid-refresh", target)
	value, err = c.Handle(t.Context(), refresh)
	if err != nil {
		t.Fatal(err)
	}
	updated := value.(protocol.Accepted)
	if updated.Resource.Version == accepted.Resource.Version || updated.ResourceGeneration <= accepted.ResourceGeneration {
		t.Fatal(updated)
	}
	select {
	case <-old.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("old discovery instance not reclaimed")
	}
	retained, err := c.Store.PluginCatalog(t.Context(), query)
	if err != nil || !reflect.DeepEqual(original, retained) {
		t.Fatal("historical catalog changed", retained, err)
	}
	query.Version = ""
	current, err := c.Store.PluginCatalog(t.Context(), query)
	if err != nil || current.Version != updated.Resource.Version || current.Definitions[0].Ref == original.Definitions[0].Ref {
		t.Fatal(current, err)
	}
	// Duplicate identities are scoped to an Agent, including exact-version reads.
	other := protocol.CreateAgent{Cwd: filepath.Dir(source)}
	create, _ := protocol.NewRequest("agent.create", "other", other)
	otherAgent, err := c.Store.Create(t.Context(), create, other, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: otherAgent.AgentID, ID: query.ID, Version: current.Version}); err == nil {
		t.Fatal("another Agent read unpublished plugin")
	}
	live := c.pluginSlots[target].instance
	c.CloseResources()
	select {
	case <-live.Closed():
	default:
		t.Fatal("Core shutdown left plugin alive")
	}
	if err := c.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(filepath.Dir(source), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := New(reopened, c.Config)
	defer replacement.CloseResources()
	status, err = replacement.resourceStatus(t.Context(), target)
	if err != nil || status.State != "unavailable" || status.Error == nil || status.Version != current.Version {
		t.Fatal("saved publication reported live", status, err)
	}
	if _, err := replacement.Store.PluginCatalog(t.Context(), query); err != nil {
		t.Fatal("offline catalog unavailable", err)
	}
	// Replaying an accepted refresh is read-only even after Core replacement.
	if _, err := replacement.Handle(t.Context(), refresh); err != nil || len(replacement.pluginSlots) != 0 {
		t.Fatal("receipt replay restarted plugin", err)
	}
	refresh, _ = protocol.NewRequest("resource.refresh", "restart-refresh", target)
	if _, err := replacement.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	status, err = replacement.resourceStatus(t.Context(), target)
	if err != nil || status.State != "ready" || status.Version != current.Version {
		t.Fatal(status, err)
	}
}

func TestPluginPublicationSupersededAndClosedHost(t *testing.T) {
	for _, mode := range []string{"superseded", "closed"} {
		t.Run(mode, func(t *testing.T) {
			c, agent, manifest, source := runtimePluginFixture(t)
			saveRuntimePlugin(t, source, manifest)
			identity, err := resource.Identity("plugin", source)
			if err != nil {
				t.Fatal(err)
			}
			target := protocol.ResourceTarget{AgentID: agent, ID: identity.ID}
			req, _ := protocol.NewRequest("resource.refresh", "slow", target)
			attempt, _, err := c.Store.BeginResourceUpdate(t.Context(), req, agent, identity, c.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			prepared, instance, err := resource.PreparePluginPackage(t.Context(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			var current *plugin.Instance
			if mode == "closed" {
				c.CloseResources()
			} else {
				manifest.Version = "2"
				saveRuntimePlugin(t, source, manifest)
				register, _ := protocol.NewRequest("resource.register", "newer", protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source})
				if _, err := c.Handle(t.Context(), register); err != nil {
					t.Fatal(err)
				}
				current = c.pluginSlots[target].instance
			}
			if _, err := c.publishPlugin(t.Context(), req, agent, prepared, instance, attempt); err == nil {
				t.Fatal("obsolete candidate published")
			}
			select {
			case <-instance.Closed():
			default:
				t.Fatal("rejected candidate left running")
			}
			if mode == "superseded" && (c.pluginSlots[target].instance != current || current.Info().State != "ready") {
				t.Fatal("newer instance changed")
			}
		})
	}
}
