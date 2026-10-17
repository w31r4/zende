package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

// Tasks and idle summaries resolve the same explicit selection and defaults.
// A plugin selection pins its published definition, not a process lease.
func (c *Core) initialModelSelection(ctx context.Context, agent string, choice store.Selection, configuration runtimeConfiguration) (string, model.Binding, error) {
	selection := choice.Choice.Model
	if selection == "" {
		selection = configuration.effective.Config.Model
	}
	var binding model.Binding
	var err error
	if len(choice.Binding) > 0 {
		binding, err = c.restoreBinding(ctx, agent, choice.Binding)
	} else if choice.Choice.ModelRevision > 0 {
		err = protocol.E("model_snapshot_invalid", "explicit model selection has no saved binding")
	} else if strings.HasPrefix(selection, "plugin:") {
		binding, err = c.resolveModelSelection(ctx, agent, selection, choice.Choice.Effort, "")
	} else if configuration.models != nil {
		binding, err = configuration.models.Resolve(ctx, selection)
	} else {
		err = protocol.E("model_unavailable", "model selection is unavailable")
	}
	if err == nil && (binding.Adapter == nil || binding.Model == "" || binding.Protocol == "") {
		err = protocol.E("model_unavailable", "selected model has no adapter")
	}
	return selection, binding, err
}

func (c *Core) modelResolver() model.Resolver {
	return c.configuration().models
}

func (c *Core) restoreBinding(ctx context.Context, agent string, raw json.RawMessage) (model.Binding, error) {
	inner, mode, err := model.SplitContextBinding(raw)
	if err != nil {
		return model.Binding{}, err
	}
	if mode != "" {
		binding, err := c.restoreBinding(ctx, agent, inner)
		if err != nil {
			return model.Binding{}, err
		}
		binding.ContextMode, binding.Snapshot = mode, append(json.RawMessage(nil), raw...)
		return binding, nil
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &header) == nil && header.Kind != "" {
		var snapshot pluginModelSnapshot
		if protocol.StrictDecode(raw, &snapshot) != nil || header.Kind != "plugin-provider" {
			return model.Binding{}, protocol.E("model_snapshot_invalid", "invalid plugin model binding")
		}
		return c.bindPluginModel(ctx, agent, snapshot)
	}
	r := c.configuration().restore
	if r == nil {
		return model.Binding{}, protocol.E("model_unavailable", "saved model selection cannot be restored")
	}
	return r.Restore(ctx, raw)
}

func (c *Core) selectAgent(ctx context.Context, req protocol.Request, p protocol.SelectAgent) (protocol.Accepted, error) {
	// Fence the catalog through durable selection. Slow configuration
	// preparation is outside this lock; only publication competes here.
	c.configMu.Lock()
	defer c.configMu.Unlock()
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	current, next, binding, err := c.prepareSelection(ctx, p)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if binding != nil {
		issues, _, _, err := c.selectionCompatibility(ctx, p.AgentID, *binding)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if len(issues) != 0 {
			return protocol.Accepted{}, issues[0]
		}
		if selected, ok := binding.Adapter.(*pluginModelSelection); ok {
			// Fence publication until the durable selection is committed.
			c.pluginMu.Lock()
			defer c.pluginMu.Unlock()
			if !c.selectionPluginReady(selected) {
				return protocol.Accepted{}, protocol.E("catalog_changed", "plugin changed while selecting a model; reopen the model catalog")
			}
		}
	}
	return c.Store.Select(ctx, req, next, current.Choice.Revision, p.Model != nil)
}

func selectionNotice(s taskSettings) string {
	mode := outputCategoryInstruction(s.NativeCategories)
	// JSON quoting keeps paths/model IDs from forging metadata lines. This is
	// host metadata, not project-file instructions or resolved credentials.
	value, _ := json.Marshal(struct {
		Cwd         string `json:"cwd"`
		Model       string `json:"model"`
		Effort      string `json:"effort"`
		ContextMode string `json:"context_mode"`
	}{s.Cwd, s.Selection, s.Effort, model.ContextModeName(s.ContextMode)})
	if s.ContextMode == model.PublicContext {
		mode += " Incompatible provider-private state stays in storage; requests use validated public history, not cross-provider hidden-reasoning continuation."
	}
	return fmt.Sprintf("Runtime selection for subsequent requests: %s. These settings supersede earlier run metadata and output-category instructions. %s Existing processes and completed tool effects remain in their original working directories; do not rerun them to move them.", value, mode)
}

func (c *Core) requestSelection(ctx context.Context, agent string, current taskSettings, binding model.Binding) (taskSettings, model.Binding, error) {
	next, err := c.Store.Selection(ctx, agent)
	if err != nil {
		return current, binding, err
	}
	if next.Choice.Revision < current.SelectionRevision {
		return current, binding, recoveryInvalid()
	}
	if next.Choice.Revision == current.SelectionRevision {
		return current, binding, nil
	}
	if next.Choice.ModelRevision != current.ModelRevision {
		binding, err = c.restoreBinding(ctx, agent, next.Binding)
		if err != nil {
			return current, binding, err
		}
		if binding.Adapter == nil || binding.Model == "" || binding.Protocol == "" {
			return current, binding, protocol.E("model_unavailable", "saved selection has no adapter")
		}
		current.Selection, current.Effort = next.Choice.Model, next.Choice.Effort
		current.Model, current.Protocol, current.NativeCategories, current.Images = binding.Model, binding.Protocol, binding.NativeCategories, binding.Images
		current.ToolSearch = binding.ToolSearch
		current.ContextMode = binding.ContextMode
		current.MaxOutputTokens, current.Binding = binding.MaxOutputTokens, binding.Snapshot
		current.ContextWindow = binding.ContextWindow
	}
	current.Version = 2
	current.Cwd, current.SelectionRevision, current.ModelRevision = next.Choice.Cwd, next.Choice.Revision, next.Choice.ModelRevision
	return current, binding, nil
}
