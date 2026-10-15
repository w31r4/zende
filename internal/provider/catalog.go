package provider

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model/responses"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func supportsProtocol(name string) bool { return name == responses.Protocol }

func connectionInfo(id string, c resource.Connection) protocol.ConnectionInfo {
	return protocol.ConnectionInfo{ID: id, Label: c.Label, Vendor: c.Vendor, Protocol: c.Protocol, Models: len(c.Models), Supported: supportsProtocol(c.Protocol)}
}

func (r *Resolver) catalogQuery(ctx context.Context, after, version string, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(after) > 1<<20 || !utf8.ValidString(after) || strings.ContainsRune(after, 0) || limit < 0 || limit > 100 {
		return 0, protocol.E("invalid_params", "catalog cursor is invalid or limit exceeds 100")
	}
	if version != "" && version != r.version {
		return 0, protocol.E("catalog_changed", "model configuration changed; restart selection")
	}
	if limit == 0 {
		limit = 20
	}
	return limit, nil
}

func (r *Resolver) Connections(ctx context.Context, q protocol.ConnectionQuery) (protocol.ConnectionPage, error) {
	limit, err := r.catalogQuery(ctx, q.After, q.Version, q.Limit)
	if err != nil {
		return protocol.ConnectionPage{}, err
	}
	out := protocol.ConnectionPage{Items: []protocol.ConnectionInfo{}, Version: r.version, DefaultModel: r.config.Model, Effort: r.config.Effort}
	ids := make([]string, 0, len(r.config.Connections))
	for id := range r.config.Connections {
		if id > q.After {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) > limit {
		out.Next = ids[limit-1]
		ids = ids[:limit]
	}
	for _, id := range ids {
		out.Items = append(out.Items, connectionInfo(id, r.config.Connections[id]))
	}
	return catalogPage(out)
}

func (r *Resolver) ListModels(ctx context.Context, q protocol.ModelQuery) (protocol.ModelPage, error) {
	limit, err := r.catalogQuery(ctx, q.After, q.Version, q.Limit)
	if err != nil {
		return protocol.ModelPage{}, err
	}
	c, ok := r.config.Connections[q.Connection]
	if !ok {
		return protocol.ModelPage{}, protocol.E("model_unavailable", "connection is not configured")
	}
	out := protocol.ModelPage{Connection: connectionInfo(q.Connection, c), Items: []protocol.ModelInfo{}, Version: r.version}
	for _, m := range c.Models {
		if m.ID > q.After {
			out.Items = append(out.Items, protocol.ModelInfo{ID: m.ID, Label: m.Label, ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput, Efforts: append([]string{}, m.Efforts...), Images: m.Images, NativeCategories: m.NativeCategories})
		}
	}
	slices.SortFunc(out.Items, func(a, b protocol.ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	if len(out.Items) > limit {
		out.Next = out.Items[limit-1].ID
		out.Items = out.Items[:limit]
	}
	return catalogPage(out)
}

func catalogPage[T any](page T) (T, error) {
	data, err := json.Marshal(page)
	if err != nil || len(data) > 1<<20 {
		var empty T
		return empty, protocol.E("catalog_limit", "model catalog page exceeds 1 MiB; use a smaller page or shorter metadata")
	}
	return page, nil
}
