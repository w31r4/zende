package runtime

import (
	"context"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

// ModelCatalog is published with the provider configuration. Catalog queries
// remain available when no default model is selected and the loop is disabled.
type ModelCatalog interface {
	Connections(context.Context, protocol.ConnectionQuery) (protocol.ConnectionPage, error)
	ListModels(context.Context, protocol.ModelQuery) (protocol.ModelPage, error)
}

func (c *Core) directory(ctx context.Context, p protocol.DirectoryQuery) (protocol.DirectoryPage, error) {
	agent, err := c.Store.Agent(ctx, p.AgentID)
	if err != nil {
		return protocol.DirectoryPage{}, err
	}
	return workspace.Directory(ctx, agent.Cwd, p)
}

func (c *Core) gitStatus(ctx context.Context, p protocol.GitQuery) (protocol.GitStatus, error) {
	agent, err := c.Store.Agent(ctx, p.AgentID)
	if err != nil {
		return protocol.GitStatus{}, err
	}
	return workspace.Git(ctx, agent.Cwd, p.Path)
}
