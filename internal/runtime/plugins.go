package runtime

import (
	"context"
	"slices"
	"strings"
	"sync"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

const MaxRetiringPluginInstances = 32

type pluginSlot struct {
	version    string
	generation int64
	instance   *plugin.Instance
	references int
	retiring   bool
	closing    bool
}

// Negotiation and schema discovery happen outside this publication lock.
// Publication and pointer replacement are serialized with Core shutdown.
func (c *Core) publishPlugin(ctx context.Context, req protocol.Request, agent string, p resource.Package, instance *plugin.Instance, attempt store.ResourcePreparation) (protocol.Accepted, error) {
	owned := false
	defer func() {
		if !owned {
			instance.Close()
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
	target := protocol.ResourceTarget{AgentID: agent, ID: p.Info.ID}
	old := c.pluginSlots[target]
	// Freeze this decision before publication. If an unchanged instance exits
	// concurrently, a later refresh may restore it; this refresh never grows the
	// retired set after checking its capacity for a different replacement plan.
	reuse := old != nil && old.version == p.Info.Version && old.instance.Info().State == "ready"
	_, oldOwned := c.pluginOwned[old]
	if oldOwned && !reuse {
		count := 0
		for slot, owner := range c.pluginOwned {
			if owner == target && slot.retiring {
				count++
			}
		}
		if count >= MaxRetiringPluginInstances {
			if accepted, found, err := c.Store.AcceptedRequest(ctx, req); err != nil || found {
				return accepted, err
			}
			return protocol.Accepted{}, protocol.E("resource_limit", "plugin has 32 retiring instances; let existing calls finish before refreshing")
		}
	}
	accepted, err := c.Store.PublishResourceUpdate(ctx, req, agent, p, attempt)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if accepted.Resource == nil || accepted.Resource.Version != p.Info.Version || old != nil && accepted.ResourceGeneration < old.generation {
		return accepted, nil
	}
	// A racing retry with the same accepted version must not replace a healthy
	// published instance. An explicit refresh can restore an offline instance.
	if reuse {
		return accepted, nil
	}
	if c.pluginSlots == nil {
		c.pluginSlots = map[protocol.ResourceTarget]*pluginSlot{}
	}
	if c.pluginOwned == nil {
		c.pluginOwned = map[*pluginSlot]protocol.ResourceTarget{}
	}
	slot := &pluginSlot{version: p.Info.Version, generation: accepted.ResourceGeneration, instance: instance}
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
	if old != nil {
		old.retiring = true
		if old.references == 0 {
			c.closePluginLocked(old)
		}
	}
	owned = true
	return accepted, nil
}

func (c *Core) pluginStatus(target protocol.ResourceTarget, status protocol.ResourceStatus) protocol.ResourceStatus {
	c.pluginMu.Lock()
	slot, closed := c.pluginSlots[target], c.pluginsClosed
	c.pluginMu.Unlock()
	status.State = "unavailable"
	status.Error = protocol.E("unavailable", "plugin process is offline; refresh to prepare a new instance")
	if !closed && slot != nil && slot.version == status.Version {
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
	if c.pluginsClosed {
		done := c.pluginClosed
		c.pluginMu.Unlock()
		<-done
		return
	}
	c.pluginsClosed = true
	c.pluginClosed = make(chan struct{})
	for slot := range c.pluginOwned {
		c.closePluginLocked(slot)
	}
	c.pluginSlots = nil
	c.pluginMu.Unlock()
	c.pluginCleanup.Wait()
	close(c.pluginClosed)
}

// Close runs outside pluginMu. Publication, status and release never wait for
// a plugin's shutdown handshake while holding the host lock.
func (c *Core) closePluginLocked(slot *pluginSlot) {
	if !slot.closing {
		slot.closing = true
		go slot.instance.Close()
	}
}

// Acquire only the current published instance. A reference acquired before a
// refresh may finish on the old instance; later acquisitions cannot select it.
func (c *Core) leasePlugin(target protocol.ResourceTarget) (*pluginSlot, func(), error) {
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	slot := c.pluginSlots[target]
	if c.pluginsClosed || slot == nil || slot.retiring || slot.closing || slot.instance.Info().State != "ready" {
		return nil, nil, protocol.E("unavailable", "plugin process is unavailable; inspect resource status and refresh before a new call")
	}
	slot.references++
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.pluginMu.Lock()
			defer c.pluginMu.Unlock()
			slot.references--
			if slot.retiring && slot.references == 0 {
				c.closePluginLocked(slot)
			}
		})
	}
	return slot, release, nil
}

type PluginInstanceStatus struct {
	Instance        plugin.Info `json:"instance"`
	ResourceVersion string      `json:"resource_version"`
	Generation      int64       `json:"generation"`
	References      int         `json:"references"`
	Retiring        bool        `json:"retiring"`
}

type PluginInstances struct {
	AgentID     string                 `json:"agent_id"`
	ResourceID  string                 `json:"resource_id"`
	Current     string                 `json:"current_instance_id,omitempty"`
	Items       []PluginInstanceStatus `json:"items"`
	MaxRetiring int                    `json:"max_retiring_instances"`
}

func (c *Core) pluginInstances(ctx context.Context, target protocol.ResourceTarget) (PluginInstances, error) {
	out := PluginInstances{AgentID: target.AgentID, ResourceID: target.ID, Items: []PluginInstanceStatus{}, MaxRetiring: MaxRetiringPluginInstances}
	status, err := c.Store.ResourceStatus(ctx, target)
	if err != nil {
		return out, err
	}
	if status.Kind != "plugin" {
		return out, protocol.E("invalid_params", "resource is not a plugin")
	}
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	if current := c.pluginSlots[target]; current != nil {
		if _, owned := c.pluginOwned[current]; owned {
			out.Current = current.instance.Info().InstanceID
		}
	}
	for slot, owner := range c.pluginOwned {
		if owner == target {
			out.Items = append(out.Items, PluginInstanceStatus{Instance: slot.instance.Info(), ResourceVersion: slot.version, Generation: slot.generation, References: slot.references, Retiring: slot.retiring})
		}
	}
	slices.SortFunc(out.Items, func(a, b PluginInstanceStatus) int {
		return strings.Compare(a.Instance.InstanceID, b.Instance.InstanceID)
	})
	return out, nil
}
