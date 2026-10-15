package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func toolMessage(call model.ToolCall, result tool.Result, failure *protocol.Error) (model.Message, error) {
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > model.MaxBlockBytes {
		return model.Message{}, protocol.E("operation_unknown", "tool ran but its result could not be retained; inspect original operation")
	}
	content := append([]model.Block(nil), result.Content...)
	if failure != nil {
		content = append(content, model.Block{Type: "text", Text: failure.Error(), IsError: true})
	}
	if len(content) == 0 {
		content = []model.Block{{Type: "text"}}
	}
	for i := range content {
		content[i].CallID = call.ID
		content[i].IsError = content[i].IsError || failure != nil
	}
	return model.Message{Role: "tool", Content: content}, nil
}

func (c *Core) settleCalls(ctx, persist context.Context, w store.Work, request string, calls []model.ToolCall, repairOnly bool, previous []*recoveryCall, appendEvent func(string, any) error) error {
	for index, call := range calls {
		executor, descriptor, found := c.Tools.Lookup(call.Name)
		op := invocation{SchemaVersion: 2, ID: protocol.NewID("call"), RequestID: request, Call: call, Version: descriptor.Version}
		prepared := false
		var result tool.Result
		var failure *protocol.Error
		if previous != nil {
			old := previous[index]
			if old.State == "completed" {
				continue
			}
			if old.State == "unknown" || old.State == "dispatched" {
				return protocol.E("operation_unknown", "original tool effect is not verified")
			}
			if old.Invocation != nil {
				op = *old.Invocation
				prepared = true
			}
			if old.State == "skipped" {
				failure = old.Failure
			}
			if failure == nil && ctx.Err() == nil && op.Version != descriptor.Version {
				return protocol.E("operation_unknown", "original tool version is unavailable")
			}
		}
		cancelled := func() *protocol.Error {
			return protocol.E("task_cancelled", "tool was not executed: task cancellation was requested")
		}
		if ctx.Err() != nil {
			failure = cancelled()
		}
		if !prepared && failure == nil {
			kind := "tool.started"
			if repairOnly {
				kind = "tool.rejected"
			}
			if err := appendEvent(kind, op); err != nil {
				if !isFailure(err, "task_cancelled") {
					return err
				}
				failure = cancelled()
			}
		}
		if ctx.Err() != nil {
			failure = cancelled()
		}
		if failure != nil {
			if err := appendEvent("tool.skipped", op); err != nil {
				return err
			}
		} else if repairOnly {
			failure = protocol.E("output_repair_tool_forbidden", "tool was not executed: format repair accepts categorized text only")
		} else if !found {
			failure = protocol.E("tool_not_found", "requested tool is not registered")
		} else {
			if err := appendEvent("tool.dispatching", op); err != nil {
				if !isFailure(err, "task_cancelled") {
					return err
				}
				failure = cancelled()
				if err := appendEvent("tool.skipped", op); err != nil {
					return err
				}
			} else {
				var err error
				result, err = executor.Execute(ctx, tool.Call{ID: op.ID, AgentID: w.Agent.ID, TaskID: w.Task.ID, Cwd: w.Agent.Cwd, Arguments: append(json.RawMessage(nil), call.Arguments...)})
				if err != nil {
					failure = safeFailure(err, "tool_failed", "tool execution failed")
					var known *protocol.Error
					if !errors.As(err, &known) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
						failure = protocol.E("operation_unknown", "tool cancellation returned without an effect result; inspect the original operation")
					}
				}
			}
		}
		if failure != nil && (failure.Data.Kind == "transport_unknown" || failure.Data.Kind == "operation_unknown") {
			if err := appendEvent("tool.unknown", toolOutcome{Invocation: op, Result: result, Error: failure}); err != nil {
				return err
			}
			return failure
		}
		result, delivery, err := c.attachWaitResults(persist, w.Agent.ID, call, result)
		if err != nil {
			return protocol.E("persistence_failed", "wait result observations could not be read; inspect original operation")
		}
		message, err := toolMessage(call, result, failure)
		if err != nil {
			if saveErr := appendEvent("tool.unknown", map[string]any{"operation_id": op.ID, "reason": "tool result exceeds storage budget or is not encodable", "limit": model.MaxBlockBytes}); saveErr != nil {
				return saveErr
			}
			return err
		}
		if err := c.Store.RecordToolResult(persist, w.Agent.ID, w.Task.ID, request, toolOutcome{Invocation: op, Result: result, Error: failure, ResultMessages: delivery.Messages}, message, delivery); err != nil {
			return protocol.E("persistence_failed", "tool result could not be saved; inspect original operation before continuing")
		}
	}
	return nil
}

func isFailure(err error, kind string) bool {
	var known *protocol.Error
	return errors.As(err, &known) && known.Data.Kind == kind
}
