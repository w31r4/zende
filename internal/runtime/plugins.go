package runtime

import (
	"context"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

type pluginSlot struct {
	version    string
	generation int64
	instance   *plugin.Instance
}

// Negotiation and schema discovery happen outside this publication lock.
// Publication and pointer replacement are serialized with Core shutdown.
func (c *Core) publishPlugin(ctx context.Context, req protocol.Request, agent string, p resource.Package, instance *plugin.Instance, attempt store.ResourcePreparation) (protocol.Accepted, error) {
	owned := false
	var previous *plugin.Instance
	defer func() {
		if !owned {
			instance.Close()
		}
		if previous != nil {
			previous.Close()
		}
	}()
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	if c.pluginsClosed {
		return protocol.Accepted{}, protocol.E("unavailable", "plugin resource host is closed")
	}
	if instance.Info().State != "ready" {
		return protocol.Accepted{}, protocol.E("unavailable", "plugin exited before publication")
	}
	accepted, err := c.Store.PublishResourceUpdate(ctx, req, agent, p, attempt)
	if err != nil {
		return protocol.Accepted{}, err
	}
	target := protocol.ResourceTarget{AgentID: agent, ID: p.Info.ID}
	old := c.pluginSlots[target]
	if accepted.Resource == nil || accepted.Resource.Version != p.Info.Version || accepted.ResourceGeneration < old.generation {
		return accepted, nil
	}
	// A racing retry with the same accepted version must not replace a healthy
	// published instance. An explicit refresh can restore an offline instance.
	if old.instance != nil && old.version == p.Info.Version && old.instance.Info().State == "ready" {
		return accepted, nil
	}
	if c.pluginSlots == nil {
		c.pluginSlots = map[protocol.ResourceTarget]pluginSlot{}
	}
	previous = old.instance
	c.pluginSlots[target] = pluginSlot{version: p.Info.Version, generation: accepted.ResourceGeneration, instance: instance}
	owned = true
	return accepted, nil
}

func (c *Core) pluginStatus(target protocol.ResourceTarget, status protocol.ResourceStatus) protocol.ResourceStatus {
	c.pluginMu.Lock()
	slot, closed := c.pluginSlots[target], c.pluginsClosed
	c.pluginMu.Unlock()
	status.State = "unavailable"
	status.Error = protocol.E("unavailable", "plugin process is offline; refresh to prepare a new instance")
	if !closed && slot.instance != nil && slot.version == status.Version {
		info := slot.instance.Info()
		status.State, status.Error = info.State, info.Error
		if info.State != "ready" && info.Error == nil {
			status.Error = protocol.E("unavailable", "plugin process is offline; refresh to prepare a new instance")
		}
	}
	return status
}

// Only the Core lifetime owns plugin processes. Client detach is unrelated.
func (c *Core) closePlugins() {
	c.pluginMu.Lock()
	c.pluginsClosed = true
	slots := c.pluginSlots
	c.pluginSlots = nil
	c.pluginMu.Unlock()
	for _, slot := range slots {
		slot.instance.Close()
	}
}
