package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

func (c *Core) beginTreeStop(ctx context.Context, req protocol.Request, agent string) (protocol.Accepted, error) {
	if old, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	if _, ok := c.Runner.(process.StopController); !ok {
		return protocol.Accepted{}, protocol.E("unavailable", "runner does not support durable tree termination")
	}
	accepted, err := c.Store.BeginTreeStop(ctx, req, agent)
	if err != nil {
		return accepted, err
	}
	plan, err := c.Store.TreeStop(ctx, req.OperationID)
	if err != nil {
		return protocol.Accepted{}, protocol.E("transport_unknown", "tree stop accepted but its scope could not be read; inspect the original operation")
	}
	for _, member := range plan.Members {
		c.cancelTask(member.TaskID)
	}
	return accepted, nil
}

// Independent of clients and model workers. A stopped model or missing provider
// must not prevent process termination or recovery of an accepted stop.
func (c *Core) runTreeStops(ctx context.Context) error {
	var cursor int64
	for {
		page, err := c.Store.PendingTreeStops(ctx, cursor)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			cursor = 0
		}
		for _, pending := range page {
			if ctx.Err() != nil {
				return nil
			}
			step, cancel := context.WithTimeout(ctx, 5*time.Second)
			stage, failure := c.advanceTreeStop(step, pending.ID)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if stage != "" {
				var wire *protocol.Error
				if failure != nil {
					wire = safeFailure(failure, "tree_stop_pending", "tree stop could not finish this pass; original scope remains frozen")
				}
				if err := c.Store.RecordTreeStopProgress(ctx, pending.ID, stage, wire); err != nil {
					return err
				}
			}
			cursor = pending.Cursor
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *Core) advanceTreeStop(ctx context.Context, id string) (string, error) {
	plan, err := c.Store.TreeStop(ctx, id)
	if err != nil {
		return "processes", err
	}
	if plan.State == "stopped" {
		return "", nil
	}
	for _, member := range plan.Members {
		c.cancelTask(member.TaskID)
	}
	controller, ok := c.Runner.(process.StopController)
	if !ok {
		return "processes", protocol.E("unavailable", "runner does not support durable tree termination")
	}
	scope, err := c.Store.TreeStopRunnerScope(ctx, id)
	if err != nil {
		return "processes", err
	}
	runner, runnerErr := controller.FreezeStop(ctx, id, scope)
	if runnerErr == nil {
		runnerErr = runner.Matches(id, scope)
	}
	if runnerErr == nil && runner.State == "frozen" {
		runner, runnerErr = controller.AdvanceStop(ctx, id)
		if runnerErr == nil {
			runnerErr = runner.Matches(id, scope)
		}
	}
	// Git has a separate lifetime. Runner trouble must not prevent its stop.
	var workspaceErr error
	for _, worktree := range plan.Worktrees {
		if ctx.Err() != nil {
			workspaceErr = ctx.Err()
			break
		}
		if err := c.stopWorktree(ctx, id, worktree); err != nil && workspaceErr == nil {
			workspaceErr = err
		}
	}
	if runnerErr != nil {
		return "processes", runnerErr
	}
	if runner.State != "stopped" && runner.State != "released" {
		return "processes", protocol.E("tree_stopping", "original process termination is still unverified; inspect runner stop records")
	}
	if workspaceErr != nil {
		return "worktrees", workspaceErr
	}
	// Recovery runs in the existing per-Agent scheduler, never concurrently with
	// its old worker. One stable request after evidence is available wakes it.
	for _, member := range plan.Members {
		if member.TaskID == "" || c.hasTaskWorker(member.TaskID) {
			continue
		}
		a, err := c.Store.Agent(ctx, member.AgentID)
		if err != nil {
			return "tasks", err
		}
		if a.State != "unknown" {
			continue
		}
		target := protocol.RecoveryTarget{AgentID: member.AgentID, TaskID: member.TaskID}
		hash := sha256.Sum256([]byte(id + "\x00" + member.TaskID))
		req, _ := protocol.NewRequest("agent.recover", "tree-recover-"+hex.EncodeToString(hash[:]), target)
		if _, found, err := c.Store.AcceptedRequest(ctx, req); err != nil {
			return "tasks", err
		} else if found {
			continue
		}
		if _, err := c.Store.RequestRecovery(ctx, req, target); err != nil && !isFailure(err, "conflict") {
			return "tasks", err
		}
	}
	if err := c.Store.TreeStopReady(ctx, id); err != nil {
		return "tasks", err
	}
	// Release runner first while core admission is still fenced. All admitted
	// late exec IDs remain reserved by runner after release. A crash here leaves
	// core frozen; retry reads the same released record, not a new scope.
	runner, err = controller.ReleaseStop(ctx, id)
	if err != nil {
		return "release", err
	}
	if err := runner.Matches(id, scope); err != nil {
		return "release", err
	}
	if err := c.Store.CompleteTreeStop(ctx, id, runner); err != nil {
		return "release", err
	}
	return "", nil
}

func (c *Core) hasTaskWorker(id string) bool {
	c.loopMu.Lock()
	defer c.loopMu.Unlock()
	_, ok := c.workers[id]
	return ok
}

// Only explicit tree cancellation may use kernel-stop evidence to settle an
// interrupted exec. Ordinary recovery still requires its original run receipt.
func (c *Core) stoppedCall(ctx context.Context, j *recoveryJournal, executor tool.Executor, call tool.Call) (tool.Recovered, error) {
	verifier, ok := executor.(tool.StoppedRecoverer)
	if !ok || !j.Work.Agent.Paused || j.Work.Task.State != "cancelling" {
		return tool.Recovered{}, protocol.E("operation_unknown", "original tool effect remains unverified")
	}
	id, err := c.Store.ActiveTreeStop(ctx, j.Work.Agent.ID)
	if err != nil {
		return tool.Recovered{}, err
	}
	if id == "" {
		return tool.Recovered{}, protocol.E("operation_unknown", "no active tree stop covers this call")
	}
	controller, ok := c.Runner.(process.StopController)
	if !ok {
		return tool.Recovered{}, protocol.E("unavailable", "runner stop verification is unavailable")
	}
	scope, err := c.Store.TreeStopRunnerScope(ctx, id)
	if err != nil {
		return tool.Recovered{}, err
	}
	proof, err := controller.InspectStop(ctx, id)
	if err != nil {
		return tool.Recovered{}, err
	}
	if err := proof.Matches(id, scope); err != nil {
		return tool.Recovered{}, err
	}
	return verifier.RecoverStopped(ctx, call, proof)
}
