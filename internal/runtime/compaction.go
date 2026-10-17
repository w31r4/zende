package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

// Summary events must not create a fresh retry budget. The preceding ordinary
// request (or Task identity before its first request) is stable across restart.
func automaticCompactionID(agent, task, precedingRequest string) string {
	raw, _ := json.Marshal([]string{agent, task, precedingRequest})
	hash := sha256.Sum256(raw)
	return "compact_" + hex.EncodeToString(hash[:])
}

// changed tells the caller to rebuild its ordinary request: preparation,
// failure and publication all append events and invalidate its saved cursor.
func (c *Core) automaticCompaction(ctx, persist context.Context, w store.Work, precedingRequest string, settings taskSettings, binding model.Binding, trigger bool) (changed bool, err error) {
	if settings.ContextPolicy == nil {
		return false, nil
	}
	id := automaticCompactionID(w.Agent.ID, w.Task.ID, precedingRequest)
	r, err := c.Store.Compaction(persist, id)
	if isFailure(err, "not_found") {
		if !trigger {
			return false, nil
		}
		req, err := protocol.NewRequest("context.compact", id, protocol.CompactContext{AgentID: w.Agent.ID})
		if err != nil {
			return false, err
		}
		if _, err := c.Store.QueueCompaction(persist, req, w.Agent.ID, w.Task.ID, "automatic", *settings.ContextPolicy); err != nil {
			if isFailure(err, "compaction_busy") {
				return false, protocol.E("request_conflict", "another compaction was accepted; rebuild at its boundary")
			}
			return false, compactionStoreFailure(err)
		}
		changed = true
	} else if err != nil {
		return false, compactionStoreFailure(err)
	} else if r.AgentID != w.Agent.ID || r.TaskID != w.Task.ID || r.Reason != "automatic" || r.Policy != *settings.ContextPolicy {
		return false, protocol.E("persistence_failed", "automatic compaction identity differs from its retained policy")
	} else if r.State == "completed" || r.State == "failed" || r.State == "cancelled" {
		return false, nil
	}
	advanced, err := c.runCompaction(ctx, persist, w, id, settings, binding)
	return changed || advanced, err
}

// runCompaction is shared by automatic boundaries and the pending manual
// scheduler. It belongs to the Agent worker, never to an IPC request context.
func (c *Core) runCompaction(ctx, persist context.Context, w store.Work, id string, settings taskSettings, binding model.Binding) (bool, error) {
	changed := false
	for {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		r, err := c.Store.Compaction(persist, id)
		if err != nil {
			return changed, compactionStoreFailure(err)
		}
		if r.AgentID != w.Agent.ID {
			return changed, protocol.E("persistence_failed", "compaction belongs to another Agent")
		}
		if r.State != "pending" {
			if r.State == "running" {
				return changed, protocol.E("persistence_failed", "summary still has an owned attempt; inspect before retrying")
			}
			return changed, nil
		}
		if r.Attempts > 0 && settings.ModelRetry != nil {
			if err := waitModelDelay(ctx, settings.ModelRetry.Delay(r.Attempts)); err != nil {
				return changed, err
			}
		}
		snapshot, err := c.Store.ContextSnapshot(persist, w.Agent.ID)
		if err != nil {
			return changed, compactionStoreFailure(err)
		}
		a, err := c.Store.BeginCompactionAttempt(persist, id, c.InstanceID, w.Task.ID, snapshot.Cursor, settings.SelectionRevision, binding)
		if err != nil {
			if isFailure(err, "compaction_conflict") {
				current, readErr := c.Store.Compaction(persist, id)
				if readErr == nil && current.State == "cancelled" {
					return changed, protocol.E("task_cancelled", "summary was cancelled with its Agent")
				}
			}
			if isFailure(err, "request_conflict") || isFailure(err, "context_busy") || isFailure(err, "task_cancelled") || isFailure(err, "tree_stopping") {
				return changed, err
			}
			reason := ""
			switch {
			case isFailure(err, "context_not_compactable"):
				reason = "context_not_compactable"
			case isFailure(err, "context_capacity"), isFailure(err, "context_limit"):
				reason = "context_capacity"
			case isFailure(err, "context_invalid"):
				reason = "summary_invalid"
			case isFailure(err, "invalid_params"):
				reason = "model_unavailable"
			}
			if reason == "" {
				return changed, compactionStoreFailure(err)
			}
			if err := c.Store.FailPendingCompaction(persist, id, reason); err != nil {
				return changed, c.compactionTransitionFailure(persist, id, err)
			}
			return true, nil
		}
		changed = true
		err = c.generateSummary(ctx, persist, w, a, settings.Cwd, binding)
		if err == nil {
			return true, nil
		}
		if persist.Err() != nil {
			return true, persist.Err()
		}
		if ctx.Err() != nil {
			// stop already cancelled the durable operation; shutdown leaves the
			// running attempt for startup recovery, with its consumed budget.
			return true, ctx.Err()
		}
		current, readErr := c.Store.Compaction(persist, id)
		if readErr == nil && current.State == "cancelled" {
			return true, protocol.E("task_cancelled", "summary was cancelled with its Agent")
		}
		if isFailure(err, "persistence_failed") {
			return true, err
		}
		reason, retry := "model_failed", true
		switch {
		case isFailure(err, "summary_invalid"), isFailure(err, "model_protocol"), isFailure(err, "model_incomplete"):
			reason = "summary_invalid"
		case isFailure(err, "summary_limit"), isFailure(err, "output_limit"):
			reason = "summary_limit"
		case isFailure(err, "summary_not_smaller"):
			reason = "summary_not_smaller"
		case isFailure(err, "context_conflict"), isFailure(err, "request_conflict"):
			reason = "context_conflict"
		case isFailure(err, "model_unavailable"), isFailure(err, "model_dependency_unavailable"):
			reason, retry = "model_unavailable", false
		case isFailure(err, "task_cancelled"), isFailure(err, "tree_stopping"), isFailure(err, "compaction_conflict"):
			return true, compactionStoreFailure(err)
		}
		if failed := c.Store.FailCompactionAttempt(persist, id, c.InstanceID, a.Request.ID, reason, retry); failed != nil {
			return true, c.compactionTransitionFailure(persist, id, failed)
		}
		if isFailure(err, "request_conflict") {
			return true, err
		}
	}
}

func (c *Core) compactionTransitionFailure(ctx context.Context, id string, err error) error {
	if isFailure(err, "compaction_conflict") {
		r, readErr := c.Store.Compaction(ctx, id)
		if readErr == nil && r.State == "cancelled" {
			return protocol.E("task_cancelled", "summary was cancelled with its Agent")
		}
	}
	return compactionStoreFailure(err)
}

func compactionStoreFailure(err error) error {
	var known *protocol.Error
	if errors.As(err, &known) && (known.Data.Kind == "request_conflict" || known.Data.Kind == "task_cancelled" || known.Data.Kind == "tree_stopping" || known.Data.Kind == "compaction_busy") {
		return err
	}
	return protocol.E("persistence_failed", "compaction state could not be read or saved; inspect the original operation")
}

func (c *Core) generateSummary(ctx, persist context.Context, w store.Work, a store.CompactionAttempt, cwd string, binding model.Binding) error {
	if binding.Adapter == nil {
		return protocol.E("model_unavailable", "summary model has no adapter")
	}
	if validator, ok := binding.Adapter.(model.RequestValidator); ok {
		if err := validator.ValidateRequest(a.Request); err != nil {
			return protocol.E("model_unavailable", "selected model cannot accept the configured summary request or output budget")
		}
	}
	adapter, pluginBinding, release, err := c.prepareModelRequest(binding, plugin.Scope{AgentID: w.Agent.ID, TaskID: w.Task.ID, Cwd: cwd}, a.Request.ID)
	if err != nil {
		return modelDependencyFailure(binding, err)
	}
	defer release()
	var actual json.RawMessage
	if pluginBinding != nil {
		actual, err = json.Marshal(pluginBinding)
		if err != nil {
			return protocol.E("model_unavailable", "summary plugin binding cannot be saved")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.Store.DispatchCompaction(persist, a.OperationID, c.InstanceID, a.Request.ID, cwd, actual); err != nil {
		return compactionStoreFailure(err)
	}
	var collector model.Collector
	var emitErr error
	err = adapter.Stream(ctx, a.Request, func(delta model.Delta) error {
		if emitErr != nil {
			return emitErr
		}
		if size := len(delta.Text) + len(delta.Raw) + len(delta.Name) + len(delta.CallID) + len(delta.MessageID) + len(delta.Protocol); size > model.MaxBlockBytes {
			emitErr = protocol.E("summary_limit", "summary delta exceeded the storage budget")
			return emitErr
		}
		if saveErr := c.Store.RecordCompactionDelta(persist, a.OperationID, c.InstanceID, a.Request.ID, delta); saveErr != nil {
			if isFailure(saveErr, "summary_limit") || isFailure(saveErr, "summary_invalid") {
				emitErr = saveErr
			} else {
				emitErr = compactionStoreFailure(saveErr)
			}
			return emitErr
		}
		emitErr = collector.Add(delta)
		if emitErr == nil {
			emitErr = ctx.Err()
		}
		return emitErr
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if emitErr != nil {
		return emitErr
	}
	if err != nil {
		return modelDependencyFailure(binding, err)
	}
	response, err := collector.Response()
	if err != nil {
		return err
	}
	if err := response.ValidateProtocol(binding.Protocol); err != nil {
		return protocol.E("summary_invalid", "summary returned another protocol's private data")
	}
	if _, err := model.CompactionSummary(response); err != nil {
		return err
	}
	_, err = c.Store.CompleteCompactionAttempt(persist, a.OperationID, c.InstanceID, a.Request.ID)
	if err != nil && !isFailure(err, "summary_invalid") && !isFailure(err, "summary_limit") && !isFailure(err, "summary_not_smaller") && !isFailure(err, "context_conflict") {
		return compactionStoreFailure(err)
	}
	return err
}
