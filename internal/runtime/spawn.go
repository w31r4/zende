package runtime

import (
	"context"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

func (c *Core) spawnAgent(ctx context.Context, req protocol.Request, p protocol.SpawnAgent) (protocol.Accepted, error) {
	// An accepted retry does not revalidate paths, current limits or the parent's
	// now changed settings. Its durable identity is the answer.
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	settings, err := c.Store.Selection(ctx, p.ParentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	path := p.Cwd
	if path == "" {
		path = "."
	}
	cwd, err := workspace.DirectoryPath(settings.Choice.Cwd, path)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.Spawn(ctx, req, p, cwd, settings.Choice.Revision, c.Config.Config.Limits.Agents, c.Config.Config.Limits.Depth)
}
