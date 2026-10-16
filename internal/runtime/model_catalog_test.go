package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
)

func scopedCatalogFixture(t *testing.T) (*Core, protocol.ResourceTarget, plugin.Manifest, string) {
	t.Helper()
	c, agent, manifest, source := runtimePluginFixture(t)
	manifest.Capabilities = append(manifest.Capabilities, "providers")
	saveRuntimePlugin(t, source, manifest)
	req, _ := protocol.NewRequest("resource.register", "register", protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source})
	value, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return c, protocol.ResourceTarget{AgentID: agent, ID: value.(protocol.Accepted).Resource.ID}, manifest, source
}

func configuredCatalog(t *testing.T, c *Core, ids ...string) ModelCatalog {
	t.Helper()
	cfg := resource.Defaults(c.Config.Config.Cwd)
	for _, id := range ids {
		cfg.Connections[id] = resource.Connection{Protocol: "responses", BaseURL: "https://catalog.invalid/v1", Credential: "env:CATALOG_MUST_NOT_RESOLVE", Models: []resource.ModelConfig{
			{ID: "model-b", ContextWindow: 8192, MaxOutput: 1024},
			{ID: "model-a", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"low", "high"}},
		}}
	}
	if len(ids) > 0 {
		cfg.Model, cfg.Effort = ids[0]+"/model-a", "high"
	}
	r, err := provider.New(cfg, func(string) string { t.Error("catalog resolved credentials"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func catalogOtherAgent(t *testing.T, c *Core) string {
	t.Helper()
	p := protocol.CreateAgent{Cwd: c.Config.Config.Cwd}
	req, _ := protocol.NewRequest("agent.create", protocol.NewID("create"), p)
	a, err := c.Store.Create(t.Context(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a.AgentID
}

func catalogConnections(t *testing.T, c *Core, q protocol.ConnectionQuery) protocol.ConnectionPage {
	t.Helper()
	req, _ := protocol.NewRequest("model.connections", "", q)
	value, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.ConnectionPage)
}

func TestModelCatalogScopedMergeIsolationAndNoRPC(t *testing.T) {
	c, target, manifest, source := scopedCatalogFixture(t)
	manifest.ID = "another-fixture"
	source += ".another"
	saveRuntimePlugin(t, source, manifest)
	register, _ := protocol.NewRequest("resource.register", "register-another", protocol.RegisterResource{AgentID: target.AgentID, Kind: "plugin", Path: source})
	registered, err := c.Handle(t.Context(), register)
	if err != nil {
		t.Fatal(err)
	}
	secondConnection := "plugin:" + registered.(protocol.Accepted).Resource.ID + ":fixture-models"
	c.Catalog = configuredCatalog(t, c, "a0", "a1", "z0", "z1")
	other := catalogOtherAgent(t, c)
	pluginConnection := "plugin:" + target.ID + ":fixture-models"
	audit := os.Getenv("ZENDE_TEST_PLUGIN_AUDIT")
	before, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	history, err := c.Store.Snapshot(t.Context(), target.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	global := catalogConnections(t, c, protocol.ConnectionQuery{})
	base, err := c.Catalog.Connections(t.Context(), protocol.ConnectionQuery{})
	if err != nil || !reflect.DeepEqual(global, base) {
		t.Fatal(global, base, err)
	}
	var version string
	want := []string{"a0", "a1", pluginConnection, secondConnection, "z0", "z1"}
	slices.Sort(want)
	for _, limit := range []int{1, 2, 3, 4, 100} {
		var ids []string
		q := protocol.ConnectionQuery{AgentID: target.AgentID, Limit: limit}
		for pages := 0; ; pages++ {
			if pages > len(want) {
				t.Fatal("pagination did not terminate")
			}
			page := catalogConnections(t, c, q)
			if version == "" {
				version = page.Version
			}
			if page.Version != version || page.Version == global.Version || page.DefaultModel != base.DefaultModel || page.Effort != base.Effort || len(page.Items) > limit {
				t.Fatalf("invalid merged page: %+v", page)
			}
			for _, item := range page.Items {
				ids = append(ids, item.ID)
			}
			if page.Next == "" {
				break
			}
			if page.Next <= q.After || page.Next != page.Items[len(page.Items)-1].ID {
				t.Fatal("invalid page cursor", page)
			}
			q.After, q.Version = page.Next, page.Version
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("limit %d: %v", limit, ids)
		}
	}
	for _, token := range []string{version, global.Version} {
		original, err := c.modelCatalogVersion(t.Context(), target.AgentID, token)
		if err != nil || original != global.Version {
			t.Fatal("resolver version translation", original, err)
		}
	}
	// Legacy config tokens are accepted only by the translation helper. Plugin
	// selection must use the strict scoped directory check before resolving.
	if _, err := c.modelConnections(t.Context(), protocol.ConnectionQuery{AgentID: target.AgentID, Version: global.Version}); !isFailure(err, "catalog_changed") {
		t.Fatal("global token fenced plugin selection", err)
	}
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: target.AgentID, Connection: pluginConnection, Version: global.Version}); !isFailure(err, "catalog_changed") {
		t.Fatal(err)
	}
	otherPage := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: other})
	if otherPage.Version == version || len(otherPage.Items) != len(global.Items) {
		t.Fatal("Agent directories were shared", otherPage)
	}
	for _, agent := range []string{other, ""} {
		if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: agent, Connection: pluginConnection}); !isFailure(err, "model_unavailable") {
			t.Fatal("plugin leaked outside owning Agent", agent, err)
		}
	}
	if _, err := c.modelCatalogVersion(t.Context(), other, version); !isFailure(err, "catalog_changed") {
		t.Fatal("another Agent accepted scoped token", err)
	}
	query := protocol.ModelQuery{AgentID: target.AgentID, Connection: pluginConnection, Version: version, Limit: 1}
	req, _ := protocol.NewRequest("model.list", "", query)
	value, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	models := value.(protocol.ModelPage)
	saved, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: target.AgentID, ID: target.ID})
	if err != nil || !reflect.DeepEqual(models.Items, saved.Providers[0].Models) || models.Version != version || models.Connection.ID != pluginConnection || !models.Connection.Supported || models.Connection.Protocol != "plugin-model" {
		t.Fatal(models, err)
	}
	query.After = models.Items[0].ID
	if empty, err := c.modelList(t.Context(), query); err != nil || len(empty.Items) != 0 || empty.Next != "" {
		t.Fatal(empty, err)
	}
	query.Connection, query.After = "a0", ""
	first, err := c.modelList(t.Context(), query)
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "model-a" || first.Next != "model-a" || first.Version != version {
		t.Fatal(first, err)
	}
	query.After = first.Next
	second, err := c.modelList(t.Context(), query)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "model-b" || second.Next != "" {
		t.Fatal(second, err)
	}
	after, err := os.ReadFile(audit)
	if err != nil || string(before) != string(after) {
		t.Fatal("catalog reads sent plugin RPC", string(after), err)
	}
	afterHistory, err := c.Store.Snapshot(t.Context(), target.AgentID)
	if err != nil || !reflect.DeepEqual(history, afterHistory) {
		t.Fatal("catalog reads mutated Agent", err)
	}
	instances, err := c.pluginInstances(t.Context(), target)
	if err != nil || len(instances.Items) != 1 || instances.Items[0].References != 0 {
		t.Fatal("catalog retained a process lease", instances, err)
	}
}

func TestModelCatalogSelectionRejectsGlobalTokenForPlugin(t *testing.T) {
	c, target, _, _ := scopedCatalogFixture(t)
	c.Catalog = configuredCatalog(t, c, "configured")
	c.Models = c.Catalog.(*provider.Resolver)
	global := catalogConnections(t, c, protocol.ConnectionQuery{})
	scoped := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: target.AgentID})
	pluginSelection := "plugin:" + target.ID + ":fixture-models/fixture-text"
	for _, tc := range []struct {
		name, selection, version string
		reject                   bool
	}{
		{"configured-global", "configured/model-a", global.Version, false},
		{"configured-scoped", "configured/model-a", scoped.Version, false},
		{"plugin-global", pluginSelection, global.Version, true},
		{"plugin-scoped", pluginSelection, scoped.Version, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding, err := c.resolveModelSelection(t.Context(), target.AgentID, tc.selection, "", tc.version)
			if tc.reject {
				if !isFailure(err, "catalog_changed") || binding.Adapter != nil {
					t.Fatal("global token selected a plugin", binding, err)
				}
				return
			}
			if err != nil || binding.Adapter == nil {
				t.Fatal("valid selection failed", binding, err)
			}
		})
	}
}

func TestModelCatalogVersionPublicationAndOffline(t *testing.T) {
	c, target, manifest, source := scopedCatalogFixture(t)
	c.Catalog = configuredCatalog(t, c, "local")
	q := protocol.ConnectionQuery{AgentID: target.AgentID}
	before := catalogConnections(t, c, q)
	other := catalogOtherAgent(t, c)
	otherBefore := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: other})
	old, release, err := c.leasePlugin(target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	req, _ := protocol.NewRequest("resource.refresh", "replace", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	after := catalogConnections(t, c, q)
	if after.Version == before.Version || !reflect.DeepEqual(after.Items, before.Items) || !old.retiring {
		t.Fatal("publication not fenced", before, after)
	}
	otherAfter := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: other})
	if otherAfter.Version != otherBefore.Version {
		t.Fatal("another Agent's refresh invalidated catalog")
	}
	if _, err := c.modelConnections(t.Context(), protocol.ConnectionQuery{AgentID: target.AgentID, Version: before.Version}); !isFailure(err, "catalog_changed") {
		t.Fatal(err)
	}
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: target.AgentID, Connection: "local", Version: before.Version}); !isFailure(err, "catalog_changed") {
		t.Fatal(err)
	}
	if _, err := c.modelCatalogVersion(t.Context(), target.AgentID, before.Version); !isFailure(err, "catalog_changed") {
		t.Fatal(err)
	}
	// A failed replacement preserves both the ready process and its token.
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "invalid-provider")
	manifest.Version = "3"
	saveRuntimePlugin(t, source, manifest)
	req, _ = protocol.NewRequest("resource.refresh", "failed-replace", target)
	if _, err := c.Handle(t.Context(), req); err == nil {
		t.Fatal("invalid provider published")
	}
	if page := catalogConnections(t, c, q); page.Version != after.Version {
		t.Fatal("failed refresh changed directory", page)
	}
	c.pluginMu.Lock()
	current := c.pluginSlots[target]
	c.pluginMu.Unlock()
	current.instance.Close()
	offline := catalogConnections(t, c, q)
	if len(offline.Items) != 1 || offline.Items[0].ID != "local" || offline.Version == after.Version {
		t.Fatal("offline or retired instance advertised", offline)
	}
	if _, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: target.AgentID, ID: target.ID}); err != nil {
		t.Fatal("offline discovery history should remain readable", err)
	}
	c.Catalog = configuredCatalog(t, c, "changed")
	if _, err := c.modelCatalogVersion(t.Context(), target.AgentID, offline.Version); !isFailure(err, "catalog_changed") {
		t.Fatal("configuration update did not fence token", err)
	}
}

func TestModelCatalogChecksSlotOwnershipAndPublishedVersion(t *testing.T) {
	c, target, _, _ := scopedCatalogFixture(t)
	other := catalogOtherAgent(t, c)
	q := protocol.ConnectionQuery{AgentID: target.AgentID}
	healthy := catalogConnections(t, c, q)
	if len(healthy.Items) != 1 {
		t.Fatal(healthy)
	}
	c.pluginMu.Lock()
	slot := c.pluginSlots[target]
	c.pluginMu.Unlock()
	for _, mismatch := range []string{"generation", "version", "retiring", "closing", "unowned", "wrong-owner"} {
		t.Run(mismatch, func(t *testing.T) {
			c.pluginMu.Lock()
			original := *slot
			switch mismatch {
			case "generation":
				slot.generation++
			case "version":
				slot.version = strings.Repeat("0", 64)
			case "retiring":
				slot.retiring = true
			case "closing":
				slot.closing = true
			case "unowned":
				delete(c.pluginOwned, slot)
			case "wrong-owner":
				c.pluginOwned[slot] = protocol.ResourceTarget{AgentID: other, ID: target.ID}
			}
			c.pluginMu.Unlock()
			defer func() {
				c.pluginMu.Lock()
				*slot = original
				c.pluginOwned[slot] = target
				c.pluginMu.Unlock()
			}()
			page := catalogConnections(t, c, q)
			if len(page.Items) != 0 || page.Version == healthy.Version {
				t.Fatal("unpublished or unavailable slot advertised", page)
			}
		})
	}
	// A plausible in-memory slot still needs an Agent-owned publication in DB.
	foreign := protocol.ResourceTarget{AgentID: other, ID: target.ID}
	c.pluginMu.Lock()
	copy := *slot
	c.pluginSlots[foreign], c.pluginOwned[&copy] = &copy, foreign
	c.pluginMu.Unlock()
	defer func() {
		c.pluginMu.Lock()
		delete(c.pluginSlots, foreign)
		delete(c.pluginOwned, &copy)
		c.pluginMu.Unlock()
	}()
	if page := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: other}); len(page.Items) != 0 {
		t.Fatal("slot bypassed publication ownership", page)
	}
}

func TestModelCatalogSameVersionRestartAndHostClose(t *testing.T) {
	c, target, _, _ := scopedCatalogFixture(t)
	q := protocol.ConnectionQuery{AgentID: target.AgentID}
	before := catalogConnections(t, c, q)
	c.pluginMu.Lock()
	old := c.pluginSlots[target]
	c.pluginMu.Unlock()
	old.instance.Close()
	if page := catalogConnections(t, c, q); len(page.Items) != 0 || page.Version == before.Version {
		t.Fatal("offline process remained available", page)
	}
	req, _ := protocol.NewRequest("resource.refresh", "restore-same-version", target)
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	after := catalogConnections(t, c, q)
	c.pluginMu.Lock()
	current := c.pluginSlots[target]
	c.pluginMu.Unlock()
	if current == old || current.instance.Info().InstanceID == old.instance.Info().InstanceID || !reflect.DeepEqual(before, after) {
		t.Fatal("same immutable catalog acquired a different version", before, after)
	}
	c.CloseResources()
	if page := catalogConnections(t, c, q); len(page.Items) != 0 || page.Version == before.Version {
		t.Fatal("closed host advertised plugins", page)
	}
}

type modelCatalogStub struct {
	connections func(context.Context, protocol.ConnectionQuery) (protocol.ConnectionPage, error)
	models      func(context.Context, protocol.ModelQuery) (protocol.ModelPage, error)
}

func (s modelCatalogStub) Connections(ctx context.Context, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
	return s.connections(ctx, q)
}
func (s modelCatalogStub) ListModels(ctx context.Context, q protocol.ModelQuery) (protocol.ModelPage, error) {
	return s.models(ctx, q)
}

func TestModelCatalogGlobalDelegatesUnchanged(t *testing.T) {
	connectionQuery := protocol.ConnectionQuery{After: "cursor", Limit: -1, Version: "original"}
	modelQuery := protocol.ModelQuery{Connection: "configured", After: "cursor", Limit: 101, Version: "original"}
	wantConnections := protocol.ConnectionPage{Next: "opaque", Version: "original", DefaultModel: "configured/model", Effort: "high"}
	wantModels := protocol.ModelPage{Next: "opaque", Version: "original"}
	sentinel := errors.New("original catalog failure")
	c := &Core{Catalog: modelCatalogStub{
		connections: func(_ context.Context, got protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
			if got != connectionQuery {
				t.Fatal(got)
			}
			return wantConnections, sentinel
		},
		models: func(_ context.Context, got protocol.ModelQuery) (protocol.ModelPage, error) {
			if got != modelQuery {
				t.Fatal(got)
			}
			return wantModels, sentinel
		},
	}}
	if got, err := c.modelConnections(t.Context(), connectionQuery); !reflect.DeepEqual(got, wantConnections) || err != sentinel {
		t.Fatal(got, err)
	}
	if got, err := c.modelList(t.Context(), modelQuery); !reflect.DeepEqual(got, wantModels) || err != sentinel {
		t.Fatal(got, err)
	}
	c.Catalog = nil
	if _, err := c.modelConnections(t.Context(), protocol.ConnectionQuery{}); !isFailure(err, "unavailable") {
		t.Fatal(err)
	}
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{}); !isFailure(err, "unavailable") {
		t.Fatal(err)
	}
}

func TestModelCatalogScopeEmptyVersionAndBounds(t *testing.T) {
	c, target, _, _ := scopedCatalogFixture(t)
	for _, agent := range []string{target.AgentID, ""} {
		if base, err := c.modelCatalogVersion(t.Context(), agent, ""); err != nil || base != "" {
			t.Fatal(base, err)
		}
	}
	if _, err := c.modelCatalogVersion(t.Context(), "missing-agent", ""); !isFailure(err, "not_found") {
		t.Fatal("empty version bypassed Agent validation", err)
	}
	if _, err := c.modelConnections(t.Context(), protocol.ConnectionQuery{AgentID: "missing-agent"}); !isFailure(err, "not_found") {
		t.Fatal(err)
	}
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: "missing-agent", Connection: "unknown"}); !isFailure(err, "not_found") {
		t.Fatal(err)
	}
	page := catalogConnections(t, c, protocol.ConnectionQuery{AgentID: target.AgentID})
	if base, err := c.modelCatalogVersion(t.Context(), target.AgentID, page.Version); err != nil || base != "" {
		t.Fatal("plugin-only catalog needs no config resolver", base, err)
	}
	for _, bad := range []protocol.ConnectionQuery{
		{Limit: -1}, {Limit: 101}, {After: "bad\x00cursor"}, {After: string([]byte{0xff})}, {After: strings.Repeat("x", modelCatalogPageBytes+1)},
	} {
		bad.AgentID = target.AgentID
		if _, err := c.modelConnections(t.Context(), bad); !isFailure(err, "invalid_params") {
			t.Fatal("connection pagination bound", err)
		}
		if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: target.AgentID, Connection: page.Items[0].ID, After: bad.After, Limit: bad.Limit}); !isFailure(err, "invalid_params") {
			t.Fatal("model pagination bound", err)
		}
	}
	for _, badAgent := range []string{"a\x00b", strings.Repeat("x", 129)} {
		if _, err := c.modelCatalogVersion(t.Context(), badAgent, ""); !isFailure(err, "invalid_params") {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.modelCatalogVersion(ctx, target.AgentID, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Apply the byte bound after adding the combined token, not just at the
	// underlying configuration adapter's page boundary.
	c.Catalog = modelCatalogStub{
		connections: func(_ context.Context, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
			if q.AgentID != "" || q.Version != "" {
				t.Fatal("scoped metadata leaked into config catalog", q)
			}
			return protocol.ConnectionPage{Items: []protocol.ConnectionInfo{}, Version: "base"}, nil
		},
		models: func(_ context.Context, q protocol.ModelQuery) (protocol.ModelPage, error) {
			if q.AgentID != "" || q.Version != "base" {
				t.Fatal("combined token leaked into config adapter", q)
			}
			return protocol.ModelPage{Version: "base", Items: []protocol.ModelInfo{{ID: "model", Label: strings.Repeat("x", modelCatalogPageBytes)}}}, nil
		},
	}
	page = catalogConnections(t, c, protocol.ConnectionQuery{AgentID: target.AgentID})
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: target.AgentID, Connection: "configured", Version: page.Version}); !isFailure(err, "catalog_limit") {
		t.Fatal("oversized model page accepted", err)
	}
	large := protocol.ConnectionPage{Items: []protocol.ConnectionInfo{{ID: "configured", Label: strings.Repeat("x", modelCatalogPageBytes)}}}
	if _, err := boundedModelCatalogPage(large); !isFailure(err, "catalog_limit") {
		t.Fatal("oversized connection page accepted", err)
	}
	c.Catalog = modelCatalogStub{
		connections: func(context.Context, protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
			return protocol.ConnectionPage{Items: []protocol.ConnectionInfo{}, Version: "base"}, nil
		},
		models: func(context.Context, protocol.ModelQuery) (protocol.ModelPage, error) {
			return protocol.ModelPage{Version: "changed"}, nil
		},
	}
	if _, err := c.modelList(t.Context(), protocol.ModelQuery{AgentID: target.AgentID, Connection: "configured"}); !isFailure(err, "catalog_changed") {
		t.Fatalf("mixed configuration snapshots: %v", err)
	}
}
