package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const modelCatalogPageBytes = 1 << 20

type scopedModelCatalogQuery struct {
	after      string
	limit      int
	connection string
}

type scopedModelCatalog struct {
	version     string
	connections []protocol.ConnectionInfo
	connection  *protocol.ConnectionInfo
	models      []protocol.ModelInfo
}

func modelCatalogLimit(after string, limit int) (int, error) {
	if len(after) > modelCatalogPageBytes || !utf8.ValidString(after) || strings.ContainsRune(after, 0) || limit < 0 || limit > 100 {
		return 0, protocol.E("invalid_params", "catalog cursor is invalid or limit exceeds 100")
	}
	if limit == 0 {
		limit = 20
	}
	return limit, nil
}

func boundedModelCatalogPage[T any](page T) (T, error) {
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > modelCatalogPageBytes {
		var empty T
		return empty, protocol.E("catalog_limit", "model catalog page exceeds 1 MiB; use a smaller page or shorter metadata")
	}
	return page, nil
}

func (c *Core) modelCatalogAgent(ctx context.Context, agent string) error {
	if len(agent) > 128 || !utf8.ValidString(agent) || strings.IndexFunc(agent, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
		return protocol.E("invalid_params", "invalid model catalog Agent")
	}
	_, err := c.Store.Agent(ctx, agent)
	return err
}

// No Agent preserves the original configuration-only API, including errors and
// version tokens. Scoped calls never pass their combined token to the provider.
func (c *Core) modelConnections(ctx context.Context, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
	catalog := c.configuration().catalog
	if q.AgentID == "" {
		if catalog == nil {
			return protocol.ConnectionPage{}, protocol.E("unavailable", "model catalog is not configured")
		}
		return catalog.Connections(ctx, q)
	}
	limit, err := modelCatalogLimit(q.After, q.Limit)
	if err != nil {
		return protocol.ConnectionPage{}, err
	}
	if err := c.modelCatalogAgent(ctx, q.AgentID); err != nil {
		return protocol.ConnectionPage{}, err
	}
	base, err := baseModelConnectionsFor(ctx, catalog, protocol.ConnectionQuery{After: q.After, Limit: limit})
	if err != nil {
		return protocol.ConnectionPage{}, err
	}
	scoped, err := c.readScopedModelCatalog(ctx, q.AgentID, base.Version, scopedModelCatalogQuery{after: q.After, limit: limit + 1})
	if err != nil {
		return protocol.ConnectionPage{}, err
	}
	if q.Version != "" && q.Version != scoped.version {
		return protocol.ConnectionPage{}, modelCatalogChanged()
	}
	more := base.Next != ""
	base.Items = append(base.Items, scoped.connections...)
	slices.SortFunc(base.Items, func(a, b protocol.ConnectionInfo) int { return strings.Compare(a.ID, b.ID) })
	if len(base.Items) > limit {
		base.Items, more = base.Items[:limit], true
	}
	base.Next, base.Version = "", scoped.version
	if more && len(base.Items) > 0 {
		base.Next = base.Items[len(base.Items)-1].ID
	}
	return boundedModelCatalogPage(base)
}

func (c *Core) modelList(ctx context.Context, q protocol.ModelQuery) (protocol.ModelPage, error) {
	catalog := c.configuration().catalog
	if q.AgentID == "" {
		if catalog == nil {
			return protocol.ModelPage{}, protocol.E("unavailable", "model catalog is not configured")
		}
		return catalog.ListModels(ctx, q)
	}
	limit, err := modelCatalogLimit(q.After, q.Limit)
	if err != nil {
		return protocol.ModelPage{}, err
	}
	if err := c.modelCatalogAgent(ctx, q.AgentID); err != nil {
		return protocol.ModelPage{}, err
	}
	base, err := baseModelConnectionsFor(ctx, catalog, protocol.ConnectionQuery{Limit: 1})
	if err != nil {
		return protocol.ModelPage{}, err
	}
	scoped, err := c.readScopedModelCatalog(ctx, q.AgentID, base.Version, scopedModelCatalogQuery{connection: q.Connection})
	if err != nil {
		return protocol.ModelPage{}, err
	}
	if q.Version != "" && q.Version != scoped.version {
		return protocol.ModelPage{}, modelCatalogChanged()
	}
	if strings.HasPrefix(q.Connection, "plugin:") {
		if scoped.connection == nil {
			return protocol.ModelPage{}, protocol.E("model_unavailable", "plugin provider is not ready for this Agent")
		}
		out := protocol.ModelPage{Connection: *scoped.connection, Items: []protocol.ModelInfo{}, Version: scoped.version}
		for _, item := range scoped.models {
			if item.ID > q.After {
				out.Items = append(out.Items, item)
			}
		}
		if len(out.Items) > limit {
			out.Next, out.Items = out.Items[limit-1].ID, out.Items[:limit]
		}
		return boundedModelCatalogPage(out)
	}
	if catalog == nil {
		return protocol.ModelPage{}, protocol.E("model_unavailable", "connection is not configured")
	}
	q.AgentID, q.Version, q.Limit = "", base.Version, limit
	out, err := catalog.ListModels(ctx, q)
	if err != nil {
		return protocol.ModelPage{}, err
	}
	if out.Version != base.Version {
		return protocol.ModelPage{}, modelCatalogChanged()
	}
	out.Version = scoped.version
	return boundedModelCatalogPage(out)
}

// modelCatalogVersion translates a scoped picker token into the configuration
// resolver's original token. Empty version means immediate selection: validate
// the Agent, but do not require a catalog or freeze the current directory.
// Legacy configuration tokens are also accepted for configured selections.
// Plugin selection must additionally validate a scoped token through
// modelConnections; a global configuration token cannot fence plugin changes.
func (c *Core) modelCatalogVersion(ctx context.Context, agent, version string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if agent != "" {
		if err := c.modelCatalogAgent(ctx, agent); err != nil {
			return "", err
		}
	}
	if version == "" {
		return "", nil
	}
	q := protocol.ConnectionQuery{Limit: 1}
	if agent == "" {
		q.Version = version
	}
	base, err := c.baseModelConnections(ctx, q)
	if err != nil {
		return "", err
	}
	if agent == "" {
		if base.Version != version {
			return "", modelCatalogChanged()
		}
		return base.Version, nil
	}
	if base.Version != "" && version == base.Version {
		return base.Version, nil
	}
	scoped, err := c.readScopedModelCatalog(ctx, agent, base.Version, scopedModelCatalogQuery{})
	if err != nil {
		return "", err
	}
	if scoped.version != version {
		return "", modelCatalogChanged()
	}
	return base.Version, nil
}

func modelCatalogChanged() error {
	return protocol.E("catalog_changed", "model catalog changed; restart selection")
}

func (c *Core) baseModelConnections(ctx context.Context, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
	return baseModelConnectionsFor(ctx, c.configuration().catalog, q)
}

func baseModelConnectionsFor(ctx context.Context, catalog ModelCatalog, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
	if catalog == nil {
		return protocol.ConnectionPage{Items: []protocol.ConnectionInfo{}}, nil
	}
	return catalog.Connections(ctx, q)
}

// Publication holds pluginMu across the database commit and slot replacement.
// Taking the same lock gives this read a consistent current directory. These
// are metadata reads only: no lease, discovery RPC, launch or credential lookup.
func (c *Core) readScopedModelCatalog(ctx context.Context, agent, baseVersion string, q scopedModelCatalogQuery) (scopedModelCatalog, error) {
	var out scopedModelCatalog
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	var targets []protocol.ResourceTarget
	if !c.pluginsClosed {
		for target := range c.pluginSlots {
			if target.AgentID == agent {
				targets = append(targets, target)
			}
		}
	}
	slices.SortFunc(targets, func(a, b protocol.ResourceTarget) int { return strings.Compare(a.ID, b.ID) })
	versions := []protocol.ResourceRef{}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		slot := c.pluginSlots[target]
		owner, owned := c.pluginOwned[slot]
		if slot == nil || !owned || owner != target || slot.retiring || slot.closing || slot.instance.Info().State != "ready" {
			continue
		}
		p, generation, err := c.Store.PublishedResource(ctx, target)
		if isFailure(err, "not_found") || isFailure(err, "conflict") {
			continue
		}
		if err != nil {
			return out, err
		}
		if p.Info.Kind != "plugin" || p.Plugin == nil || p.Info.Version != slot.version || generation != slot.generation || slot.instance.Info().State != "ready" || len(p.Plugin.Providers) == 0 {
			continue
		}
		versions = append(versions, protocol.ResourceRef{ID: target.ID, Version: slot.version})
		for _, d := range p.Plugin.Providers {
			info := protocol.ConnectionInfo{ID: "plugin:" + target.ID + ":" + d.Name, Label: d.Name, Protocol: "plugin-model", Models: len(d.Models), Supported: true}
			// Resource IDs and provider names are already sorted; retain only
			// enough candidates for one merged page and a next-page indicator.
			if info.ID > q.after && len(out.connections) < q.limit {
				out.connections = append(out.connections, info)
			}
			if info.ID == q.connection {
				out.connection, out.models = &info, d.Models
			}
		}
	}
	raw, _ := json.Marshal(struct {
		Agent   string                 `json:"agent"`
		Base    string                 `json:"base"`
		Plugins []protocol.ResourceRef `json:"plugins"`
	}{agent, baseVersion, versions})
	sum := sha256.Sum256(raw)
	out.version = "agent-models:" + hex.EncodeToString(sum[:])
	return out, nil
}
