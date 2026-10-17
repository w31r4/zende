package runtime

import (
	"context"
	"encoding/json"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

// Verification uses the core context: a user stop cancels execution, but the
// original effect still needs a read-only query before cancellation can settle.
func (c *Core) recoverWork(taskCtx, persist context.Context, held store.HeldTask) (err error) {
	defer func() {
		if recover() != nil {
			j, readErr := readRecoveryJournal(persist, c.Store, held.AgentID, held.TaskID)
			if readErr == nil {
				_ = c.Store.RecordRecoveryFailure(persist, held.AgentID, held.TaskID, j.Work.Agent.Seq, protocol.E("recovery_panic", "verification stopped unexpectedly; original effects remain held"))
			}
			err = nil
		}
	}()
	for attempt := 0; attempt < 4; attempt++ {
		j, err := readRecoveryJournal(persist, c.Store, held.AgentID, held.TaskID)
		if err != nil {
			if persist.Err() != nil {
				return nil
			}
			snap, snapshotErr := c.Store.Snapshot(persist, held.AgentID)
			if snapshotErr != nil {
				return snapshotErr
			}
			saveErr := c.Store.RecordRecoveryFailure(persist, held.AgentID, held.TaskID, snap.Cursor, safeFailure(err, "recovery_invalid", "saved task history could not be verified"))
			if isFailure(saveErr, "recovery_conflict") {
				continue
			}
			return saveErr
		}
		if j.Work.Agent.State != "unknown" {
			return nil
		}
		verified, failure := c.verifyCalls(persist, j)
		if persist.Err() != nil {
			return nil
		}
		if len(verified) > 0 {
			if err := c.Store.RecordVerifiedResults(persist, held.AgentID, held.TaskID, j.Work.Agent.Seq, verified); err != nil && !isFailure(err, "recovery_conflict") {
				return err
			}
			continue // Re-read paired results and the current stop decision.
		}
		var resume *resumedTask
		if failure == nil {
			resume, failure = c.prepareResume(persist, j)
		}
		if failure == nil {
			failure = c.checkResumeModelDependency(persist, resume)
		}
		if failure != nil {
			err := c.Store.RecordRecoveryFailure(persist, held.AgentID, held.TaskID, j.Work.Agent.Seq, safeFailure(failure, "recovery_unavailable", "original task cannot yet continue; inspect recovery records"))
			if isFailure(err, "recovery_conflict") {
				continue
			}
			return err
		}
		w, err := c.Store.ActivateHeld(persist, held.AgentID, held.TaskID, j.Work.Agent.Seq)
		if isFailure(err, "recovery_conflict") {
			continue
		}
		if err != nil {
			return err
		}
		if j.Settings != nil {
			w.Agent.Cwd = j.Settings.Cwd
		}
		if w.Task.State == "cancelling" {
			c.cancelTask(w.Task.ID)
		}
		return c.executeWork(taskCtx, persist, w, resume)
	}
	return nil // Contended by new decisions; explicit recover can try again.
}

func (c *Core) verifyCalls(ctx context.Context, j *recoveryJournal) ([]store.VerifiedResult, error) {
	var verified []store.VerifiedResult
	var failure error
	for _, pending := range j.Calls {
		if pending.State != "unknown" && pending.State != "dispatched" {
			continue
		}
		op := pending.Invocation
		executor, descriptor, ok := c.requestTool(j.Request, pending.Call.Name)
		if op == nil || !ok || op.Version != descriptor.Version {
			failure = protocol.E("recovery_unavailable", "original tool version is unavailable; effect remains held")
			continue
		}
		recoverer, ok := executor.(tool.Recoverer)
		if !ok {
			failure = protocol.E("recovery_unavailable", "original tool has no effect verifier; inspect its existing artifacts")
			continue
		}
		cwd := j.Work.Agent.Cwd
		if j.Settings != nil {
			cwd = j.Settings.Cwd
		}
		call := tool.Call{ID: op.ID, AgentID: j.Work.Agent.ID, TaskID: j.Work.Task.ID, Cwd: cwd, Arguments: append(json.RawMessage(nil), op.Call.Arguments...)}
		r, err := recoverer.Recover(ctx, call)
		if err != nil && pending.Call.Name == "exec_command" && j.Work.Agent.Paused && j.Work.Task.State == "cancelling" {
			if stopped, stopErr := c.stoppedCall(ctx, j, executor, call); stopErr == nil {
				r, err = stopped, nil
			}
		}
		if err != nil {
			failure = err
			continue
		}
		if r.Error != nil && (r.Error.Data.Kind == "operation_unknown" || r.Error.Data.Kind == "transport_unknown") {
			failure = r.Error
			continue
		}
		result, delivery, err := c.attachWaitResults(ctx, j.Work.Agent.ID, op.Call, r.Result)
		if err != nil {
			failure = err
			continue
		}
		message, err := toolMessage(op.Call, result, r.Error)
		if err != nil {
			failure = err
			continue
		}
		verified = append(verified, store.VerifiedResult{RequestID: op.RequestID, OperationID: op.ID, Outcome: toolOutcome{Invocation: *op, Result: result, Error: r.Error, ResultMessages: delivery.Messages}, Message: message, Delivery: delivery})
	}
	return verified, failure
}

func (c *Core) prepareResume(ctx context.Context, j *recoveryJournal) (*resumedTask, error) {
	resume := &resumedTask{Journal: j}
	// Cancellation/final need no model connection. Remaining un-dispatched
	// calls will receive cancellation results under their original identities.
	if j.Work.Agent.Paused || j.Work.Task.State == "cancelling" {
		return resume, nil
	}
	if j.ResponseComplete && j.Invalid == nil && !j.pending() {
		_, final, err := (model.Response{Output: j.Output}).Final()
		if err == nil && final {
			return resume, nil
		}
	}
	if j.Settings == nil {
		if j.Request != nil {
			return nil, protocol.E("model_snapshot_missing", "original model settings were not recorded; task remains held")
		}
		if !c.modelAvailable() {
			return nil, protocol.E("model_unavailable", "no model is configured for this not-yet-dispatched task")
		}
		return nil, nil // Claimed before any settings/request: start same Task.
	}
	if len(j.Settings.Binding) == 0 {
		return nil, protocol.E("model_snapshot_missing", "original provider binding cannot be restored")
	}
	b, err := c.restoreBinding(ctx, j.Work.Agent.ID, append(json.RawMessage(nil), j.Settings.Binding...))
	if err != nil {
		return nil, err
	}
	s := j.Settings
	if b.Adapter == nil || b.Model != s.Model || b.Protocol != s.Protocol || b.NativeCategories != s.NativeCategories || b.ToolSearch != s.ToolSearch || b.MaxOutputTokens != s.MaxOutputTokens || s.Images && !b.Images || s.ContextWindow > 0 && b.ContextWindow != s.ContextWindow {
		return nil, protocol.E("model_snapshot_invalid", "restored provider differs from the original task")
	}
	if j.Request != nil {
		if j.Request.Model != s.Model || j.Request.Protocol != s.Protocol || j.Request.MaxOutputTokens != s.MaxOutputTokens || j.Request.RepairLimit != s.FormatRepairs {
			return nil, recoveryInvalid()
		}
		for _, call := range j.Calls {
			if call.State == "completed" || call.State == "skipped" || j.Request.FormatRepair {
				continue
			}
			expected := ""
			for _, d := range j.Request.Tools {
				if d.Name == call.Call.Name {
					expected = d.Version
					break
				}
			}
			_, current, _ := c.requestTool(j.Request, call.Call.Name)
			if current.Version != expected || call.Invocation != nil && call.Invocation.Version != expected {
				return nil, protocol.E("recovery_unavailable", "pending call requires its original tool definition")
			}
		}
	}
	resume.Binding = b
	return resume, nil
}
