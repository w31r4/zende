package runtime

import (
	"context"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

func (c *Core) spawnAgent(ctx context.Context, req protocol.Request, p protocol.SpawnAgent) (protocol.Accepted, error) {
	// An accepted retry does not revalidate paths, current limits or the parent's
	// now changed settings. Its durable identity is the answer.
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	mode, err := protocol.SpawnWorkspaceMode(p.Workspace, p.WorktreePath)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if mode == "worktree" {
		return c.spawnInWorktree(ctx, req, p)
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

func (c *Core) spawnInWorktree(ctx context.Context, req protocol.Request, p protocol.SpawnAgent) (protocol.Accepted, error) {
	failure := func(err error) (protocol.Accepted, error) {
		wire := safeFailure(err, "worktree_failed", "workspace preparation failed; inspect the original operation")
		wire.Data.OperationID = req.OperationID
		return protocol.Accepted{}, wire
	}
	lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(req.OperationID))
	if err != nil {
		return failure(err)
	}
	defer lock.Close()
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	record, err := c.Store.WorktreeRecord(ctx, req)
	if err != nil {
		return failure(err)
	}
	if record == nil {
		settings, err := c.Store.Selection(ctx, p.ParentID)
		if err != nil {
			return failure(err)
		}
		source, err := workspace.DirectoryPath(settings.Choice.Cwd, p.Cwd)
		if err != nil {
			return failure(err)
		}
		plan, err := workspace.PrepareWorktree(ctx, source, p.WorktreePath)
		if err != nil {
			return failure(err)
		}
		record, err = c.Store.PrepareWorktree(ctx, req, p, plan, settings.Choice.Revision, c.Config.Config.Limits.Agents, c.Config.Config.Limits.Depth)
		if err != nil {
			return failure(err)
		}
	}
	if record.Operation.State == "prepared" {
		record, err = c.Store.TransitionWorktree(ctx, req, "prepared", "creating", nil)
		if err != nil {
			return failure(err)
		}
		if err := workspace.CreateTrackedWorktree(ctx, record.Operation.Workspace, lock, func(group process.Group) error {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			return c.Store.RecordWorktreeProcess(persist, req, group)
		}); err != nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			wire := safeFailure(err, "worktree_unknown", "Git checkout was interrupted; inspect the original operation")
			wire.Data.OperationID = req.OperationID
			if _, saveErr := c.Store.TransitionWorktree(persist, req, "creating", "unknown", wire); saveErr != nil {
				return failure(protocol.E("persistence_failed", "Git outcome could not be saved; retry only the original request"))
			}
			return failure(wire)
		}
		// Exit success is evidence of completed checkout; subsequent local edits
		// do not invalidate it. Persist before creating an executable Agent.
		record, err = c.Store.TransitionWorktree(ctx, req, "creating", "ready", nil)
		if err != nil {
			return failure(err)
		}
	} else if record.Operation.State == "creating" || record.Operation.State == "unknown" {
		if err := workspace.VerifyWorktree(ctx, record.Operation.Workspace, true); err != nil {
			if record.Operation.State == "creating" {
				wire := safeFailure(err, "worktree_unknown", "checkout could not be verified")
				wire.Data.OperationID = req.OperationID
				if _, saveErr := c.Store.TransitionWorktree(ctx, req, "creating", "unknown", wire); saveErr != nil {
					return failure(saveErr)
				}
			}
			return failure(err)
		}
		record, err = c.Store.TransitionWorktree(ctx, req, record.Operation.State, "ready", nil)
		if err != nil {
			return failure(err)
		}
	}
	if record.Operation.State != "ready" {
		if record.Operation.State == "cancelled" {
			return failure(protocol.E("cancelled", "workspace creation was cancelled; existing files are preserved"))
		}
		return failure(protocol.E("worktree_pending", "workspace is not ready"))
	}
	if err := workspace.VerifyWorktree(ctx, record.Operation.Workspace, false); err != nil {
		return failure(err)
	}
	settings, err := c.Store.Selection(ctx, record.Params.ParentID)
	if err != nil {
		return failure(err)
	}
	accepted, err := c.Store.Spawn(ctx, req, record.Params, record.Operation.Workspace.Path, settings.Choice.Revision, c.Config.Config.Limits.Agents, c.Config.Config.Limits.Depth)
	if err != nil {
		return failure(err)
	}
	return accepted, nil
}
