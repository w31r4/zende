package runtime

import (
	"context"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

type compactionWorker struct {
	id     string
	cancel context.CancelFunc
}

func (c *Core) registerCompactionWorker(ctx context.Context, agent, id string) (*compactionWorker, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	worker := &compactionWorker{id: id, cancel: cancel}
	c.loopMu.Lock()
	if c.compactionWorkers == nil {
		c.compactionWorkers = map[string]*compactionWorker{}
	}
	c.compactionWorkers[agent] = worker
	c.loopMu.Unlock()
	return worker, ctx
}

func (c *Core) forgetCompactionWorker(agent string, worker *compactionWorker) {
	worker.cancel()
	c.loopMu.Lock()
	defer c.loopMu.Unlock()
	if c.compactionWorkers[agent] == worker {
		delete(c.compactionWorkers, agent)
	}
}

func (c *Core) cancelIdleCompaction(ctx context.Context, agent string) {
	c.loopMu.Lock()
	worker := c.compactionWorkers[agent]
	c.loopMu.Unlock()
	if worker == nil {
		return
	}
	// A new explicit compact may be accepted after stop commits. Cancel only
	// the operation stop actually closed, never a newer worker on this Agent.
	r, err := c.Store.Compaction(ctx, worker.id)
	if err == nil && r.State == "cancelled" {
		worker.cancel()
	}
}

func (c *Core) manualCompaction(ctx, persist context.Context, w store.Work, settings taskSettings, binding model.Binding) (bool, error) {
	r, err := c.Store.ActiveCompaction(persist, w.Agent.ID)
	if err != nil {
		return false, compactionStoreFailure(err)
	}
	if r == nil || r.Reason != "manual" {
		return false, nil
	}
	return c.runCompaction(ctx, persist, w, r.OperationID, settings, binding)
}

// Idle summaries share the Agent slot with Tasks but never manufacture a Task,
// deliver mailbox, or change pause state. Each attempt saves its real binding.
func (c *Core) runIdleCompaction(ctx, persist context.Context, agent, id string) (err error) {
	defer func() {
		if recover() != nil {
			// Leave the attempt journal for startup's bounded recovery.
			err = protocol.E("persistence_failed", "summary worker stopped unexpectedly; inspect its retained operation")
		}
	}()
	for conflicts := 0; ; conflicts++ {
		if ctx.Err() != nil {
			return nil
		}
		r, err := c.Store.Compaction(persist, id)
		if err != nil {
			return compactionStoreFailure(err)
		}
		if r.AgentID != agent || r.Reason != "manual" {
			return protocol.E("persistence_failed", "idle summary identity changed")
		}
		if r.State == "running" {
			return protocol.E("persistence_failed", "idle summary still owns an unfinished attempt")
		}
		if r.State != "pending" {
			return nil // Also closes stop-before-worker-registration.
		}
		a, err := c.Store.Agent(persist, agent)
		if err != nil {
			return compactionStoreFailure(err)
		}
		if a.State != "idle" && a.State != "paused" {
			return nil // Its Task/recovery worker must settle first.
		}
		if conflicts >= 8 {
			return c.failIdleCompaction(persist, id, "context_conflict")
		}
		choice, err := c.Store.Selection(persist, agent)
		if err != nil {
			return compactionStoreFailure(err)
		}
		_, binding, err := c.initialModelSelection(ctx, agent, choice, c.configuration())
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return c.failIdleCompaction(persist, id, "model_unavailable")
		}
		settings := taskSettings{Cwd: choice.Choice.Cwd, SelectionRevision: choice.Choice.Revision}
		_, err = c.runCompaction(ctx, persist, store.Work{Agent: a}, id, settings, binding)
		if ctx.Err() != nil {
			return nil
		}
		if isFailure(err, "request_conflict") {
			continue
		}
		if isFailure(err, "task_cancelled") || isFailure(err, "tree_stopping") {
			return nil
		}
		if isFailure(err, "context_busy") {
			// An idle Agent with an incomplete historical group cannot make
			// progress by spinning. A newly active Task handles its own queue.
			a, readErr := c.Store.Agent(persist, agent)
			if readErr != nil {
				return compactionStoreFailure(readErr)
			}
			if a.State != "idle" && a.State != "paused" {
				return nil
			}
			return c.failIdleCompaction(persist, id, "context_not_compactable")
		}
		return err
	}
}

func (c *Core) failIdleCompaction(ctx context.Context, id, reason string) error {
	err := c.Store.FailPendingCompaction(ctx, id, reason)
	if err == nil || isFailure(err, "compaction_conflict") {
		return nil // A concurrent stop already made it terminal.
	}
	return compactionStoreFailure(err)
}
