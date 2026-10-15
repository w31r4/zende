package runtime

import (
	"context"
	"path/filepath"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func (c *Core) registerResource(ctx context.Context, req protocol.Request, p protocol.RegisterResource) (protocol.Accepted, error) {
	if accepted, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return accepted, err
	}
	snapshot, err := c.Store.Snapshot(ctx, p.AgentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if p.Path == "" {
		return protocol.Accepted{}, protocol.E("invalid_params", "resource path is required")
	}
	if !filepath.IsAbs(p.Path) {
		p.Path = filepath.Join(snapshot.Agent.Cwd, p.Path)
	}
	current, err := c.Store.Resources(ctx, protocol.ResourceList{AgentID: p.AgentID, Limit: 1})
	if err != nil {
		return protocol.Accepted{}, err
	}
	prepared, err := resource.Prepare(ctx, p.Kind, p.Path)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.PublishResource(ctx, req, p.AgentID, prepared, current.Generation)
}

func (c *Core) refreshResource(ctx context.Context, req protocol.Request, p protocol.ResourceTarget) (protocol.Accepted, error) {
	if accepted, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return accepted, err
	}
	current, err := c.Store.ResourceStatus(ctx, p)
	if err != nil {
		return protocol.Accepted{}, err
	}
	prepared, err := resource.Prepare(ctx, current.Kind, current.Source)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.PublishResource(ctx, req, p.AgentID, prepared, current.Generation)
}
