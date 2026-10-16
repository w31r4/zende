package runtime

import (
	"context"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

const MaxActivePluginCalls = 64

type pluginExecution struct {
	task    string
	cancel  context.CancelFunc
	failure *protocol.Error
}

// Entry adapters supply a verified scope. The initiating client owns only the
// acceptance wait, never the lifetime of an accepted plugin operation.
func (c *Core) startPluginCall(ctx context.Context, req protocol.Request, scope plugin.Scope) (store.PluginReceipt, error) {
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	prior, err := c.Store.PriorPluginCall(ctx, req)
	if err != nil {
		return store.PluginReceipt{}, err
	}
	if prior != nil {
		if worker := c.pluginCalls[prior.Receipt.PluginCallTarget]; worker != nil && worker.failure != nil {
			return store.PluginReceipt{}, worker.failure
		}
		return prior.Receipt, nil
	}
	if c.pluginCallsClosed {
		return store.PluginReceipt{}, protocol.E("unavailable", "plugin execution host is closed")
	}
	if scope.AgentID == "" && req.Method == "plugin.command.call" {
		scope, err = c.Store.PluginCommandScope(ctx, req)
		if err != nil {
			return store.PluginReceipt{}, err
		}
	}
	for target, worker := range c.pluginCalls {
		if target.AgentID == scope.AgentID && worker.failure != nil {
			return store.PluginReceipt{}, protocol.E("persistence_failed", "an earlier plugin outcome could not be saved; inspect the original operation before new work")
		}
	}
	if len(c.pluginCalls) >= MaxActivePluginCalls {
		return store.PluginReceipt{}, protocol.E("resource_limit", "Core has 64 active or unpersisted plugin calls; inspect existing operations before starting another")
	}
	call, inv, release, err := c.preparePluginCall(req, scope)
	if err != nil {
		return store.PluginReceipt{}, err
	}
	record, fresh, err := c.Store.BeginPluginCall(ctx, inv, c.InstanceID)
	if err != nil || !fresh {
		release()
		if err != nil {
			return store.PluginReceipt{}, err
		}
		return record.Receipt, nil
	}
	callCtx, cancel := context.WithCancel(context.Background())
	worker := &pluginExecution{task: scope.TaskID, cancel: cancel}
	if c.pluginCalls == nil {
		c.pluginCalls = map[protocol.PluginCallTarget]*pluginExecution{}
	}
	target := record.Receipt.PluginCallTarget
	c.pluginCalls[target] = worker
	c.pluginCallWorkers.Add(1)
	go c.runPluginCall(callCtx, target, worker, call, release)
	return record.Receipt, nil
}

func (c *Core) runPluginCall(ctx context.Context, target protocol.PluginCallTarget, worker *pluginExecution, call *plugin.Call, release func()) {
	defer c.pluginCallWorkers.Done()
	defer worker.cancel()
	defer release()
	result := invokePluginCall(ctx, call, func(p plugin.Progress) error {
		persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.Store.AppendPluginProgress(persist, target, c.InstanceID, p)
	})
	persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Store.FinishPluginCall(persist, target, c.InstanceID, result)
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	if err != nil {
		// Keep a bounded fault marker. Never advertise an uncommitted result or
		// grant this Agent another dispatch after persistence became uncertain.
		worker.failure = protocol.E("persistence_failed", "plugin outcome could not be saved; original operation remains unverified")
	} else {
		delete(c.pluginCalls, target)
	}
}

func invokePluginCall(ctx context.Context, call *plugin.Call, progress func(plugin.Progress) error) (result plugin.CallResult) {
	result = plugin.CallResult{CallBinding: call.Binding(), State: "unknown"}
	var requestID string
	defer func() {
		// Preserve the original binding even if a host adapter unexpectedly
		// panics after a side effect. Arbitrary panic values are not diagnostics.
		if recover() != nil {
			result = plugin.CallResult{CallBinding: call.Binding(), RequestID: requestID, State: "unknown"}
		}
	}()
	result, _ = call.Invoke(ctx, func(p plugin.Progress) error {
		requestID = p.RequestID
		return progress(p)
	})
	return result
}

func (c *Core) pluginCallFailure(target protocol.PluginCallTarget) error {
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	if worker := c.pluginCalls[target]; worker != nil && worker.failure != nil {
		return worker.failure
	}
	return nil
}

func (c *Core) cancelPluginCall(ctx context.Context, req protocol.Request) (protocol.Accepted, error) {
	accepted, err := c.Store.CancelPluginCall(ctx, req)
	if err != nil {
		return accepted, err
	}
	var target protocol.PluginCallTarget
	if err := protocol.StrictDecode(req.Params, &target); err != nil {
		return protocol.Accepted{}, err
	}
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	if worker := c.pluginCalls[target]; worker != nil {
		worker.cancel()
	}
	return accepted, nil
}

// Called only after a tree stop has committed cancellation and frozen new
// dispatch for this Agent. Ordinary Task stops preserve unrelated commands.
func (c *Core) cancelAgentPluginCalls(agent string) {
	c.pluginCallMu.Lock()
	defer c.pluginCallMu.Unlock()
	for target, worker := range c.pluginCalls {
		if target.AgentID == agent {
			worker.cancel()
		}
	}
}

func (c *Core) closePluginCalls() {
	c.pluginCallMu.Lock()
	if c.pluginCallsClosed {
		done := c.pluginCallsDone
		c.pluginCallMu.Unlock()
		<-done
		return
	}
	c.pluginCallsClosed = true
	c.pluginCallsDone = make(chan struct{})
	for _, worker := range c.pluginCalls {
		worker.cancel()
	}
	c.pluginCallMu.Unlock()
	c.pluginCallWorkers.Wait()
	close(c.pluginCallsDone)
}

func (c *Core) waitPluginTask(ctx context.Context, agent, task string) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		c.pluginCallMu.Lock()
		failed := false
		for target, worker := range c.pluginCalls {
			if target.AgentID == agent && worker.task == task && worker.failure != nil {
				failed = true
				break
			}
		}
		c.pluginCallMu.Unlock()
		if failed {
			return protocol.E("persistence_failed", "Task has an uncommitted plugin outcome; inspect its original operation")
		}
		status, err := c.Store.PluginTaskStatus(ctx, agent, task)
		if err != nil {
			return err
		}
		if status.Unknown != 0 {
			return protocol.E("operation_unknown", "Task has an unverified plugin outcome; inspect its original operation")
		}
		if status.Pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
