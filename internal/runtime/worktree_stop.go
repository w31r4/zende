package runtime

import (
	"context"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

// One bounded pass of an already captured tree stop. No new checkout, directory
// removal or child publication occurs here. Repetition/restart uses saved kernel
// lifetimes and the same inherited lock, not the original Git command.
func (c *Core) stopWorktree(ctx context.Context, stop, id string) error {
	record, err := c.Store.WorktreeStop(ctx, stop, id)
	if err != nil {
		return err
	}
	if record.Group != nil && !record.Stopped {
		group, ended, err := process.ObserveGroup(*record.Group)
		if err != nil {
			return err
		}
		record.Group, record.Stopped = &group, ended
		if err := c.Store.RecordWorktreeStop(ctx, record); err != nil {
			return err // Never signal before saving evidence for the next core.
		}
		if !ended {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := process.KillGroup(group); err != nil {
				return err
			}
			return protocol.E("worktree_busy", "workspace termination requested; waiting for process exit and lock release")
		}
	}
	lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(id))
	if err != nil {
		return err
	}
	defer lock.Close()
	// Store re-reads the evidence inside the transaction: a late Start identity
	// cannot be missed between the initial query and lock acquisition.
	return c.Store.CancelWorktree(ctx, stop, id)
}
