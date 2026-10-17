package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

type pluginModelSnapshot struct {
	Kind          string               `json:"kind"`
	Version       int                  `json:"version"`
	Selection     string               `json:"selection"`
	Resource      protocol.ResourceRef `json:"resource"`
	DefinitionRef string               `json:"definition_ref"`
	Model         string               `json:"model"`
	Effort        string               `json:"effort,omitempty"`
}

// Process identity belongs to a dispatched request, not the stable selection.
// A refresh can recreate the same resource version on a new process.
type pluginModelRequestBinding struct {
	Resource   protocol.ResourceRef `json:"resource"`
	Generation int64                `json:"generation"`
	plugin.CallBinding
}

// This selection owns no process reference. Each request acquires and releases
// its own reference, including all validation and journal-conflict paths.
type pluginModelSelection struct {
	core      *Core
	agent     string
	snapshot  pluginModelSnapshot
	validator *plugin.ModelAdapter
}

func (a *pluginModelSelection) ValidateRequest(r model.Request) error {
	return a.validator.ValidateRequest(r)
}
func (a *pluginModelSelection) ValidateContext(r model.Request) error {
	return a.validator.ValidateContext(r)
}
func (a *pluginModelSelection) Stream(context.Context, model.Request, func(model.Delta) error) error {
	return protocol.E("model_unavailable", "plugin model request must acquire its instance before journaling")
}

func parsePluginModelSelection(selection string) (resource, provider, name string, ok bool) {
	connection, name, found := strings.Cut(selection, "/")
	if !found || name == "" {
		return "", "", "", false
	}
	parts := strings.Split(connection, ":")
	if len(parts) != 3 || parts[0] != "plugin" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[1], parts[2], name, true
}

func (c *Core) resolveModelSelection(ctx context.Context, agent, selection, effort, version string) (model.Binding, error) {
	baseVersion := ""
	if version != "" {
		var err error
		if strings.HasPrefix(selection, "plugin:") {
			// Global configuration tokens do not fence plugin definitions.
			_, err = c.modelConnections(ctx, protocol.ConnectionQuery{AgentID: agent, Version: version, Limit: 1})
		} else {
			baseVersion, err = c.modelCatalogVersion(ctx, agent, version)
		}
		if err != nil {
			return model.Binding{}, err
		}
	}
	if strings.HasPrefix(selection, "plugin:") {
		id, provider, name, ok := parsePluginModelSelection(selection)
		if !ok {
			return model.Binding{}, protocol.E("invalid_params", "plugin model uses plugin:RESOURCE:PROVIDER/MODEL")
		}
		catalog, err := c.Store.PluginCatalog(ctx, protocol.PluginCatalogQuery{AgentID: agent, ID: id})
		if err != nil {
			return model.Binding{}, err
		}
		for _, d := range catalog.Providers {
			if d.Name == provider {
				return c.bindPluginModel(ctx, agent, pluginModelSnapshot{Kind: "plugin-provider", Version: 1, Selection: selection,
					Resource: protocol.ResourceRef{ID: id, Version: catalog.Version}, DefinitionRef: d.Ref, Model: name, Effort: effort})
			}
		}
		return model.Binding{}, protocol.E("model_unavailable", "plugin provider is absent from the current catalog")
	}
	r, ok := c.modelResolver().(model.SelectionResolver)
	if !ok {
		return model.Binding{}, protocol.E("model_unavailable", "configured model selection is unavailable")
	}
	return r.ResolveSelection(ctx, selection, effort, baseVersion)
}

func (c *Core) bindPluginModel(ctx context.Context, agent string, s pluginModelSnapshot) (model.Binding, error) {
	id, provider, name, valid := parsePluginModelSelection(s.Selection)
	if !valid || s.Kind != "plugin-provider" || s.Version != 1 || s.Resource.ID != id || s.Resource.Version == "" || s.Model != name {
		return model.Binding{}, protocol.E("model_snapshot_invalid", "invalid plugin model selection snapshot")
	}
	// This exact read checks that this Agent actually published the version;
	// a copied binding cannot borrow another Agent's plugin process.
	catalog, err := c.Store.PluginCatalog(ctx, protocol.PluginCatalogQuery{AgentID: agent, ID: id, Version: s.Resource.Version})
	if err != nil {
		return model.Binding{}, err
	}
	var definition *plugin.ProviderDefinition
	for _, d := range catalog.Providers {
		if d.Name == provider && d.Ref == s.DefinitionRef {
			definition = &d
			break
		}
	}
	if definition == nil {
		return model.Binding{}, protocol.E("model_snapshot_invalid", "plugin provider differs from its saved definition")
	}
	a, err := plugin.ModelMetadata(catalog.Source, *definition, s.Model, s.Effort)
	if err != nil {
		return model.Binding{}, err
	}
	info := a.Info()
	raw, _ := json.Marshal(s)
	return model.Binding{Adapter: &pluginModelSelection{core: c, agent: agent, snapshot: s, validator: a}, Model: info.ID, Protocol: a.Protocol(), NativeCategories: info.NativeCategories, Images: info.Images, MaxOutputTokens: info.MaxOutput, ContextWindow: info.ContextWindow, Snapshot: raw}, nil
}

func (c *Core) prepareModelRequest(b model.Binding, scope plugin.Scope, requestID string) (model.Adapter, *pluginModelRequestBinding, func(), error) {
	a, ok := b.Adapter.(*pluginModelSelection)
	if !ok {
		return b.Adapter, nil, func() {}, nil
	}
	if a.core != c || a.agent != scope.AgentID {
		return nil, nil, nil, protocol.E("model_unavailable", "plugin model selection belongs to another Agent")
	}
	slot, release, err := c.leasePlugin(protocol.ResourceTarget{AgentID: scope.AgentID, ID: a.snapshot.Resource.ID})
	if err != nil {
		return nil, nil, nil, err
	}
	if slot.version != a.snapshot.Resource.Version {
		release()
		return nil, nil, nil, protocol.E("stale_reference", "plugin model was replaced; explicitly select a ready version")
	}
	adapter, err := slot.instance.Model(a.snapshot.DefinitionRef, a.snapshot.Model, a.snapshot.Effort, scope)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	actual := &pluginModelRequestBinding{Resource: a.snapshot.Resource, Generation: slot.generation, CallBinding: adapter.Binding()}
	actual.OperationID = requestID
	return adapter, actual, release, nil
}

// Metadata restoration remains possible offline. Before activating a held
// Task, check the selection its next request will actually use, including an
// explicit upgrade. No model request is sent and no lease survives this check.
func (c *Core) checkResumeModelDependency(ctx context.Context, resume *resumedTask) error {
	if resume == nil || resume.Binding.Adapter == nil || resume.Journal.Settings == nil {
		return nil // Cancellation, a complete final, or a not-yet-started Task.
	}
	j := resume.Journal
	s, b, err := c.requestSelection(ctx, j.Work.Agent.ID, *j.Settings, resume.Binding)
	if err != nil {
		return err
	}
	if _, ok := b.Adapter.(*pluginModelSelection); !ok {
		return nil
	}
	_, _, release, err := c.prepareModelRequest(b, plugin.Scope{AgentID: j.Work.Agent.ID, TaskID: j.Work.Task.ID, Cwd: s.Cwd}, "")
	if release != nil {
		release()
	}
	return err
}

func modelDependencyFailure(b model.Binding, err error) error {
	if _, ok := b.Adapter.(*pluginModelSelection); ok && (isFailure(err, "unavailable") || isFailure(err, "stale_reference")) {
		// Readiness can change after recovery's check or between tool rounds.
		// Preserve the original Task for refresh/reselection and explicit recover.
		return protocol.E("model_dependency_unavailable", "plugin model is offline or replaced; refresh its exact version or explicitly select a ready model, then recover this Task")
	}
	return err
}

func (c *Core) modelAvailable() bool {
	if c.modelResolver() != nil {
		return true
	}
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	for _, slot := range c.pluginSlots {
		if !slot.closing && !slot.retiring && slot.instance.Info().State == "ready" {
			for _, capability := range slot.instance.Info().Capabilities {
				if capability == "providers" {
					return true
				}
			}
		}
	}
	return false
}

func validPluginModelRecord(r requestRecord) bool {
	isPlugin := strings.HasPrefix(r.Protocol, "plugin-model/")
	if r.PluginBinding == nil {
		return !isPlugin
	}
	p := r.PluginBinding
	if !isPlugin || r.Settings == nil || r.ResourceCatalog == nil || p.Generation < 1 || p.Generation > r.ResourceCatalog.Generation || p.OperationID != r.ID || p.InstanceID == "" || p.PluginID == "" || p.PluginVersion == "" || p.ManifestVersion == "" || p.CatalogVersion == "" {
		return false
	}
	var snapshot pluginModelSnapshot
	if protocol.StrictDecode(r.Settings.Binding, &snapshot) != nil || snapshot.Kind != "plugin-provider" || snapshot.Version != 1 || snapshot.Resource != p.Resource || snapshot.DefinitionRef != p.DefinitionRef || snapshot.Model != r.Model || snapshot.Selection != r.Settings.Selection || snapshot.Effort != r.Settings.Effort {
		return false
	}
	for _, ref := range r.ResourceCatalog.Entries {
		if ref == p.Resource {
			return true
		}
	}
	return false
}
