package runtime

import (
	"context"
	"reflect"
	"slices"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

func (c *Core) publishPluginResource(ctx context.Context, req protocol.Request, agent, source string, attempt store.ResourcePreparation) (protocol.Accepted, error) {
	parent, kind, name, err := protocol.ParsePluginResourceSource(source)
	if err != nil {
		return protocol.Accepted{}, err
	}
	target := protocol.ResourceTarget{AgentID: agent, ID: parent}
	slot, release, err := c.leasePlugin(target)
	if err != nil {
		return protocol.Accepted{}, err
	}
	defer release()
	published, generation, err := c.Store.PublishedResource(ctx, target)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if published.Plugin == nil || published.Info.Kind != "plugin" || published.Info.Version != slot.version || generation != slot.generation {
		return protocol.Accepted{}, protocol.E("resource_changed", "plugin changed during resource preparation; refresh again")
	}
	n := slices.IndexFunc(published.Plugin.Resources, func(d plugin.ResourceDefinition) bool { return d.Kind == kind && d.Name == name })
	if n < 0 {
		return protocol.Accepted{}, protocol.E("invalid_resource", "plugin does not provide this resource; inspect its catalog")
	}
	expected := published.Plugin.Resources[n]
	definition, read, err := slot.instance.ReadResource(ctx, expected.Ref)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if !reflect.DeepEqual(expected, definition) {
		return protocol.Accepted{}, protocol.E("resource_changed", "plugin resource catalog differs from its publication")
	}
	files := make([]resource.File, len(read))
	for n, f := range read {
		files[n] = resource.File{Path: f.Path, Data: f.Data, Executable: f.Executable}
	}
	origin := protocol.PluginResourceOrigin{Plugin: protocol.ResourceRef{ID: parent, Version: slot.version}, DefinitionRef: expected.Ref}
	p, err := resource.BuildPluginResourcePackage(source, origin, expected, files)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.PublishResourceUpdate(ctx, req, agent, p, attempt)
}
