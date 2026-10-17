package runtime

import (
	"context"
	"strings"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/workspace"
)

func (c *Core) beginSessionCopy(ctx context.Context, req protocol.Request, p protocol.CopyAgent) (protocol.Accepted, error) {
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	if !p.Confirm {
		return protocol.Accepted{}, protocol.E("invalid_params", "confirm copying explicitly; the source Agent tree and its owned processes will stop")
	}
	if _, ok := c.Runner.(process.StopController); !ok {
		return protocol.Accepted{}, protocol.E("unavailable", "session copy requires durable runner tree termination")
	}
	choice, err := c.Store.Selection(ctx, p.AgentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if p.Cwd == "" {
		return protocol.Accepted{}, protocol.E("invalid_params", "copy requires an existing target directory")
	}
	cwd, err := workspace.DirectoryPath(choice.Choice.Cwd, p.Cwd)
	if err != nil {
		return protocol.Accepted{}, err
	}
	accepted, err := c.Store.BeginSessionCopy(ctx, req, cwd, choice.Choice.Revision)
	if err != nil {
		return accepted, err
	}
	plan, err := c.Store.TreeStop(ctx, req.OperationID)
	if err != nil {
		return protocol.Accepted{}, protocol.E("transport_unknown", "copy accepted but its stop scope could not be read; inspect the original operation")
	}
	for _, member := range plan.Members {
		c.cancelTask(member.TaskID)
		c.cancelIdleCompaction(ctx, member.AgentID)
		c.cancelAgentPluginCalls(member.AgentID)
	}
	return accepted, nil
}

func (c *Core) runSessionCopies(ctx context.Context) error {
	var cursor int64
	type retry struct {
		at    time.Time
		delay time.Duration
	}
	retries := map[string]retry{}
	for {
		page, err := c.Store.PendingSessionCopies(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(page) == 0 {
			cursor = 0
		}
		for _, pending := range page {
			if ctx.Err() != nil {
				return nil
			}
			if last := retries[pending.ID]; time.Now().Before(last.at) {
				cursor = pending.Cursor
				continue
			}
			if err := c.advanceSessionCopy(ctx, pending.ID); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				failure := safeFailure(err, "copy_pending", "copy publication could not finish; source history and the original copy identity are retained")
				if err := c.Store.RecordSessionCopyFailure(ctx, pending.ID, failure); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				delay := retries[pending.ID].delay * 2
				if delay == 0 {
					delay = 2 * time.Second
				}
				delay = min(delay, 30*time.Second)
				retries[pending.ID] = retry{at: time.Now().Add(delay), delay: delay}
			} else {
				delete(retries, pending.ID)
			}
			cursor = pending.Cursor
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *Core) advanceSessionCopy(ctx context.Context, id string) error {
	r, err := c.Store.SessionCopy(ctx, id)
	if err != nil {
		return err
	}
	if r.Info.State != "copying" {
		return nil
	} // Tree stop has its own durable worker.
	if err := c.Store.SessionCopyReady(ctx, id); err != nil {
		return err
	}
	plan, err := c.Store.TreeStop(ctx, id)
	if err != nil {
		return err
	}
	for _, member := range plan.Members {
		c.loopMu.Lock()
		_, compacting := c.compactionWorkers[member.AgentID]
		_, working := c.workers[member.TaskID]
		c.loopMu.Unlock()
		if working || compacting {
			return protocol.E("copy_workers_pending", "source workers are finishing cancellation")
		}
	}
	// Recheck the retained target at publication, not at a fresh relative cwd.
	if _, err := workspace.DirectoryPath(r.Info.Cwd, "."); err != nil {
		return err
	}
	cleanup, stop := context.WithTimeout(ctx, 5*time.Second)
	err = c.closeCopiedResourceHosts(cleanup, plan)
	stop()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(r.Selection.Choice.Model, "plugin:") {
		_, _, err = c.Store.PublishSessionCopy(ctx, id, nil)
		return err
	}
	// A selected plugin provider needs its own new instance. Reuse spawn's
	// exact-version preparation and hold publication against execution leases.
	choice, err := c.Store.Selection(ctx, r.Info.SourceAgent)
	if err != nil {
		return err
	}
	prepare, cancel := context.WithTimeout(ctx, 15*time.Second)
	dependency, instance, err := c.prepareSpawnDependency(prepare, r.Info.SourceAgent, choice)
	cancel()
	if err != nil {
		if isFailure(err, "stale_reference") {
			return protocol.E("stale_reference", "selected plugin source changed; restore its retained version before this copy can publish")
		}
		return err
	}
	owned := false
	defer func() {
		if !owned {
			instance.Close()
		}
	}()
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	if c.pluginsClosed || instance.Info().State != "ready" {
		return protocol.E("unavailable", "copied model provider is not ready for publication")
	}
	info, publication, err := c.Store.PublishSessionCopy(ctx, id, dependency)
	if err != nil || publication == nil {
		return err
	}
	c.installCopiedModelPlugin(info.AgentID, *publication, instance)
	owned = true
	return nil
}

// Caller holds pluginMu through the durable commit and pointer installation.
func (c *Core) installCopiedModelPlugin(agent string, p store.SpawnDependencyPublication, instance *plugin.Instance) {
	target := protocol.ResourceTarget{AgentID: agent, ID: p.Resource.ID}
	slot := &pluginSlot{version: p.Resource.Version, generation: p.Generation, instance: instance}
	if c.pluginSlots == nil {
		c.pluginSlots = map[protocol.ResourceTarget]*pluginSlot{}
	}
	if c.pluginOwned == nil {
		c.pluginOwned = map[*pluginSlot]protocol.ResourceTarget{}
	}
	c.pluginSlots[target], c.pluginOwned[slot] = slot, target
	c.pluginCleanup.Add(1)
	go func() {
		defer c.pluginCleanup.Done()
		<-instance.Closed()
		c.pluginMu.Lock()
		delete(c.pluginOwned, slot)
		c.pluginMu.Unlock()
	}()
}

func (c *Core) closeCopiedResourceHosts(ctx context.Context, plan store.TreeStopRecord) error {
	agents := map[string]bool{}
	for _, member := range plan.Members {
		agents[member.AgentID] = true
	}
	c.pluginMu.Lock()
	var plugins []*pluginSlot
	for slot, owner := range c.pluginOwned {
		if !agents[owner.AgentID] {
			continue
		}
		if slot.references != 0 {
			c.pluginMu.Unlock()
			return protocol.E("copy_workers_pending", "source plugin leases are still closing")
		}
		plugins = append(plugins, slot)
	}
	for _, slot := range plugins {
		slot.retiring = true
		c.closePluginLocked(slot)
	}
	c.pluginMu.Unlock()
	for _, slot := range plugins {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-slot.instance.Closed():
		}
	}
	c.pluginMu.Lock()
	for target, slot := range c.pluginSlots {
		if agents[target.AgentID] && slot.closing {
			delete(c.pluginSlots, target)
		}
	}
	c.pluginMu.Unlock()
	c.mcpMu.Lock()
	var mcps []*mcpSlot
	for target, slot := range c.mcpSlots {
		if agents[target.AgentID] {
			mcps = append(mcps, slot)
		}
	}
	c.mcpMu.Unlock()
	closed := map[*mcpSlot]bool{}
	for _, slot := range mcps {
		if err := slot.lock(ctx); err != nil {
			return err
		}
		if slot.cancel != nil {
			slot.cancel()
		}
		connections := []*resource.MCPConnection{}
		if slot.conn != nil {
			connections = append(connections, slot.conn)
		}
		for conn := range slot.retired {
			connections = append(connections, conn)
		}
		if slot.retired == nil {
			slot.retired = map[*resource.MCPConnection]struct{}{}
		}
		for _, conn := range connections {
			slot.retired[conn] = struct{}{}
		}
		slot.conn, slot.snapshot = nil, nil
		for _, conn := range connections {
			go func(conn *resource.MCPConnection) { _ = conn.Close() }(conn)
		}
		slot.unlock()
		for _, conn := range connections {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-conn.Closed():
			}
		}
		if err := slot.lock(ctx); err != nil {
			return err
		}
		for _, conn := range connections {
			delete(slot.retired, conn)
		}
		slot.pkg = resource.Package{}
		slot.unlock()
		closed[slot] = true
	}
	c.mcpMu.Lock()
	for target, slot := range c.mcpSlots {
		if agents[target.AgentID] && closed[slot] {
			delete(c.mcpSlots, target)
		}
	}
	c.mcpMu.Unlock()
	return nil
}
