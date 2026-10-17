package runtime

import (
	"bytes"
	"context"
	"strings"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

func (c *Core) acceptedSpawn(ctx context.Context, req protocol.Request) (protocol.Accepted, bool, error) {
	// A retry must not expose the committed child before its instance has been
	// installed. Execution leases take the same lock.
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	return c.Store.AcceptedRequest(ctx, req)
}

func (c *Core) spawnWithModelDependency(ctx context.Context, req protocol.Request, p protocol.SpawnAgent, cwd string, settings store.Selection) (protocol.Accepted, error) {
	limits := c.configuration().effective.Config.Limits
	if !strings.HasPrefix(settings.Choice.Model, "plugin:") {
		return c.Store.Spawn(ctx, req, p, cwd, settings.Choice.Revision, limits.Agents, limits.Depth)
	}
	dependency, instance, err := c.prepareSpawnDependency(ctx, p.ParentID, settings)
	if err != nil {
		return protocol.Accepted{}, err
	}
	owned := false
	defer func() {
		if !owned {
			instance.Close()
		}
	}()
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	// Another caller may have committed this operation during preparation. Its
	// immutable receipt wins, even if shutdown or a parent edit happened later.
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	if c.pluginsClosed || instance.Info().State != "ready" {
		return protocol.Accepted{}, protocol.E("unavailable", "child plugin is unavailable before spawn publication")
	}
	accepted, publication, err := c.Store.SpawnWithDependency(ctx, req, p, cwd, settings.Choice.Revision, limits.Agents, limits.Depth, dependency)
	if err != nil || publication == nil {
		return accepted, err
	}
	// No fallible work remains after commit. The candidate is now owned solely
	// by this child, and normal Core shutdown/refcounting owns its lifecycle.
	target := protocol.ResourceTarget{AgentID: accepted.AgentID, ID: publication.Resource.ID}
	slot := &pluginSlot{version: publication.Resource.Version, generation: publication.Generation, instance: instance}
	if c.pluginSlots == nil {
		c.pluginSlots = map[protocol.ResourceTarget]*pluginSlot{}
	}
	if c.pluginOwned == nil {
		c.pluginOwned = map[*pluginSlot]protocol.ResourceTarget{}
	}
	c.pluginSlots[target] = slot
	c.pluginOwned[slot] = target
	c.pluginCleanup.Add(1)
	go func() {
		defer c.pluginCleanup.Done()
		<-instance.Closed()
		c.pluginMu.Lock()
		delete(c.pluginOwned, slot)
		c.pluginMu.Unlock()
	}()
	owned = true
	return accepted, nil
}

func (c *Core) prepareSpawnDependency(ctx context.Context, parent string, settings store.Selection) (*store.SpawnDependency, *plugin.Instance, error) {
	// Restore validates retained metadata only; it neither borrows the parent's
	// live instance nor asserts that the child has one.
	binding, err := c.restoreBinding(ctx, parent, settings.Binding)
	if err != nil {
		return nil, nil, err
	}
	selected, ok := binding.Adapter.(*pluginModelSelection)
	if !ok || selected.snapshot.Selection != settings.Choice.Model || selected.snapshot.Effort != settings.Choice.Effort {
		return nil, nil, protocol.E("model_snapshot_invalid", "parent selection differs from its plugin model binding")
	}
	snapshot := selected.snapshot
	catalog, err := c.Store.PluginCatalog(ctx, protocol.PluginCatalogQuery{AgentID: parent, ID: snapshot.Resource.ID, Version: snapshot.Resource.Version})
	if err != nil {
		return nil, nil, err
	}
	// Reuse standard manifest/credential validation and negotiation. This may
	// start only an exact match of the selected snapshot, never silently use a
	// newer source/catalog. Packages retain manifest/catalog, not executable bytes.
	p, instance, err := resource.PreparePluginPackage(ctx, catalog.Source, nil)
	if err != nil {
		return nil, nil, err
	}
	if p.Info.ID != snapshot.Resource.ID || p.Info.Version != snapshot.Resource.Version {
		instance.Close()
		return nil, nil, protocol.E("stale_reference", "plugin source or catalog changed; reselect a ready model before spawning")
	}
	// Validate the exact provider ref/model/effort without sending a request.
	if _, err := instance.Model(snapshot.DefinitionRef, snapshot.Model, snapshot.Effort, plugin.Scope{AgentID: parent, Cwd: settings.Choice.Cwd}); err != nil {
		instance.Close()
		return nil, nil, err
	}
	return &store.SpawnDependency{Binding: bytes.Clone(settings.Binding), Package: p}, instance, nil
}
