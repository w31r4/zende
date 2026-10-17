package runtime

import (
	"context"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/workspace"
)

// Called with configMu held. Preview and commit prepare the same candidate;
// preview never stores a selection, takes a process lease, or sends a request.
func (c *Core) prepareSelection(ctx context.Context, p protocol.SelectAgent) (store.Selection, store.Selection, *model.Binding, error) {
	var current, next store.Selection
	if p.Cwd == nil && p.Model == nil || p.Effort != nil && p.Model == nil || p.CatalogVersion != "" && p.Model == nil {
		return current, next, nil, protocol.E("invalid_params", "select cwd or model; effort and catalog_version require model")
	}
	current, err := c.Store.Selection(ctx, p.AgentID)
	if err != nil {
		return current, next, nil, err
	}
	if p.ExpectedRevision != nil && *p.ExpectedRevision != current.Choice.Revision {
		return current, next, nil, protocol.E("selection_changed", "Agent settings changed; inspect and select again")
	}
	next = current
	if p.Cwd != nil {
		next.Choice.Cwd, err = workspace.DirectoryPath(current.Choice.Cwd, *p.Cwd)
		if err != nil {
			return current, next, nil, err
		}
	}
	if p.Model == nil {
		return current, next, nil, nil
	}
	effort := ""
	if p.Effort != nil {
		effort = *p.Effort
	}
	b, err := c.resolveModelSelection(ctx, p.AgentID, *p.Model, effort, p.CatalogVersion)
	if err != nil {
		return current, next, nil, safeFailure(err, "model_unavailable", "selected model is unavailable")
	}
	if b.Adapter == nil || b.Model == "" || b.Protocol == "" || len(b.Snapshot) == 0 {
		return current, next, nil, protocol.E("model_unavailable", "selected model has no restorable binding")
	}
	next.Choice.Model, next.Choice.Effort, next.Binding = *p.Model, effort, b.Snapshot
	return current, next, &b, nil
}

func (c *Core) selectionCompatibility(ctx context.Context, agent string, b model.Binding) ([]*protocol.Error, int64, error) {
	messages, cursor, err := c.Store.ModelContext(ctx, agent)
	if err != nil {
		return nil, 0, err
	}
	issues := []*protocol.Error{}
	add := func(err error) {
		if err == nil {
			return
		}
		issue := safeFailure(err, "model_incompatible", "current context cannot be used by this model; history is retained")
		for _, prior := range issues {
			if prior.Data.Kind == issue.Data.Kind && prior.Message == issue.Message {
				return
			}
		}
		issues = append(issues, issue)
	}
	images, imageErr := c.resolveImages(ctx, agent, messages, b.Images)
	add(imageErr)
	req := model.Request{Model: b.Model, Messages: messages, Images: images, MaxOutputTokens: b.MaxOutputTokens}
	if c.Tools != nil {
		descriptors, err := c.searchDescriptors(c.Tools.Descriptors(), b.ToolSearch)
		add(err)
		for _, d := range descriptors {
			req.Tools = append(req.Tools, d.Tool)
		}
	} else if b.ToolSearch != "" {
		add(protocol.E("unsupported_capability", "native search requires the registered MCP discovery executor"))
	}
	if b.ToolSearch != "" {
		discovered, err := discoverMCPHistory(messages)
		add(err)
		for _, d := range discovered {
			if c.Tools != nil {
				if _, _, exists := c.Tools.Lookup(d.Tool.Name); exists {
					add(protocol.E("tool_conflict", "native MCP name conflicts with a host executor"))
					break
				}
			}
		}
	}
	if validator, ok := b.Adapter.(model.ContextValidator); ok {
		if imageErr == nil {
			add(validator.ValidateContext(req))
		}
	} else {
		add(protocol.E("unsupported_capability", "selected adapter cannot inspect context compatibility without a request"))
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return issues, cursor, nil
}

// The caller holds pluginMu; commit keeps this fence until Store.Select ends.
func (c *Core) selectionPluginReady(selected *pluginModelSelection) bool {
	slot := c.pluginSlots[protocol.ResourceTarget{AgentID: selected.agent, ID: selected.snapshot.Resource.ID}]
	return !c.pluginsClosed && slot != nil && slot.version == selected.snapshot.Resource.Version && !slot.closing && !slot.retiring && slot.instance.Info().State == "ready"
}

func (c *Core) previewSelection(ctx context.Context, p protocol.SelectAgent) (protocol.SelectionPreview, error) {
	var result protocol.SelectionPreview
	if p.Model == nil {
		return result, protocol.E("invalid_params", "selection preview requires a model")
	}
	c.configMu.Lock()
	defer c.configMu.Unlock()
	current, next, candidate, err := c.prepareSelection(ctx, p)
	if err != nil {
		return result, err
	}
	result = protocol.SelectionPreview{AgentID: p.AgentID, Revision: current.Choice.Revision,
		CurrentCwd: current.Choice.Cwd, Cwd: next.Choice.Cwd, Changes: []string{},
		Candidate: selectionModel(next.Choice.Model, next.Choice.Effort, *candidate)}
	result.Issues, result.Cursor, err = c.selectionCompatibility(ctx, p.AgentID, *candidate)
	if err != nil {
		return result, err
	}
	if selected, ok := candidate.Adapter.(*pluginModelSelection); ok {
		c.pluginMu.Lock()
		ready := c.selectionPluginReady(selected)
		c.pluginMu.Unlock()
		if !ready {
			result.Issues = append(result.Issues, protocol.E("catalog_changed", "plugin changed while selecting a model; reopen the model catalog"))
		}
	}
	configuration := c.configuration()
	selection, binding, err := c.initialModelSelection(ctx, p.AgentID, current, configuration)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.CurrentError = safeFailure(err, "model_unavailable", "current model binding is unavailable")
	} else {
		effort := current.Choice.Effort
		if current.Choice.ModelRevision == 0 && len(current.Binding) == 0 {
			effort = configuration.effective.Config.Effort
		}
		info := selectionModel(selection, effort, binding)
		result.Current = &info
		result.Changes = selectionChanges(info, result.Candidate)
	}
	if current.Choice.Cwd != next.Choice.Cwd {
		result.Changes = append(result.Changes, "cwd")
	}
	result.Compatible = len(result.Issues) == 0
	return result, nil
}

func selectionModel(selection, effort string, b model.Binding) protocol.SelectionModel {
	search := b.ToolSearch
	if search == "" {
		search = "host"
	}
	return protocol.SelectionModel{Selection: selection, Model: b.Model, Protocol: b.Protocol, Effort: effort,
		ContextWindow: b.ContextWindow, MaxOutput: b.MaxOutputTokens, Images: b.Images, NativeCategories: b.NativeCategories, ToolSearch: search}
}

func selectionChanges(a, b protocol.SelectionModel) []string {
	changes := []string{}
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"selection", a.Selection != b.Selection}, {"model", a.Model != b.Model}, {"protocol", a.Protocol != b.Protocol},
		{"effort", a.Effort != b.Effort}, {"context_window", a.ContextWindow != b.ContextWindow}, {"max_output", a.MaxOutput != b.MaxOutput},
		{"images", a.Images != b.Images}, {"native_categories", a.NativeCategories != b.NativeCategories}, {"tool_search", a.ToolSearch != b.ToolSearch},
	} {
		if field.changed {
			changes = append(changes, field.name)
		}
	}
	return changes
}
