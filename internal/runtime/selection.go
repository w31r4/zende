package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

func (c *Core) modelResolver() model.Resolver {
	if c.Models != nil {
		return c.Models
	}
	r, _ := c.RestoreModels.(model.Resolver)
	return r
}

func (c *Core) restoreBinding(ctx context.Context, agent string, raw json.RawMessage) (model.Binding, error) {
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
	r := c.RestoreModels
	if r == nil {
		r, _ = c.Models.(model.Restorer)
	}
	if r == nil {
		return model.Binding{}, protocol.E("model_unavailable", "saved model selection cannot be restored")
	}
	return r.Restore(ctx, raw)
}

func (c *Core) selectAgent(ctx context.Context, req protocol.Request, p protocol.SelectAgent) (protocol.Accepted, error) {
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	if p.Cwd == nil && p.Model == nil || p.Effort != nil && p.Model == nil || p.CatalogVersion != "" && p.Model == nil {
		return protocol.Accepted{}, protocol.E("invalid_params", "select cwd or model; effort and catalog_version require model")
	}
	current, err := c.Store.Selection(ctx, p.AgentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if p.ExpectedRevision != nil && *p.ExpectedRevision != current.Choice.Revision {
		return protocol.Accepted{}, protocol.E("selection_changed", "Agent settings changed; inspect and select again")
	}
	next := current
	if p.Cwd != nil {
		next.Choice.Cwd, err = workspace.DirectoryPath(current.Choice.Cwd, *p.Cwd)
		if err != nil {
			return protocol.Accepted{}, err
		}
	}
	if p.Model != nil {
		effort := ""
		if p.Effort != nil {
			effort = *p.Effort
		}
		b, err := c.resolveModelSelection(ctx, p.AgentID, *p.Model, effort, p.CatalogVersion)
		if err != nil {
			return protocol.Accepted{}, safeFailure(err, "model_unavailable", "selected model is unavailable")
		}
		if b.Adapter == nil || b.Model == "" || b.Protocol == "" || len(b.Snapshot) == 0 {
			return protocol.Accepted{}, protocol.E("model_unavailable", "selected model has no restorable binding")
		}
		messages, _, err := c.Store.ModelContext(ctx, p.AgentID)
		if err != nil {
			return protocol.Accepted{}, err
		}
		images, err := c.resolveImages(ctx, p.AgentID, messages, b.Images)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if validator, ok := b.Adapter.(model.ContextValidator); ok {
			if err := validator.ValidateContext(model.Request{Model: b.Model, Messages: messages, Images: images, MaxOutputTokens: b.MaxOutputTokens}); err != nil {
				return protocol.Accepted{}, safeFailure(err, "model_incompatible", "current context cannot be used by this model")
			}
		}
		next.Choice.Model, next.Choice.Effort, next.Binding = *p.Model, effort, b.Snapshot
		if selected, ok := b.Adapter.(*pluginModelSelection); ok {
			// Fence publication until the durable selection is committed.
			c.pluginMu.Lock()
			defer c.pluginMu.Unlock()
			slot := c.pluginSlots[protocol.ResourceTarget{AgentID: p.AgentID, ID: selected.snapshot.Resource.ID}]
			if c.pluginsClosed || slot == nil || slot.version != selected.snapshot.Resource.Version || slot.closing || slot.retiring || slot.instance.Info().State != "ready" {
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
		Cwd    string `json:"cwd"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}{s.Cwd, s.Selection, s.Effort})
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
		current.MaxOutputTokens, current.Binding = binding.MaxOutputTokens, binding.Snapshot
	}
	current.Version = 2
	current.Cwd, current.SelectionRevision, current.ModelRevision = next.Choice.Cwd, next.Choice.Revision, next.Choice.ModelRevision
	return current, binding, nil
}
