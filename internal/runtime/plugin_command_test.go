package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

func pluginCommandRequest(t *testing.T, c *Core, target protocol.ResourceTarget, op string) (protocol.Request, protocol.PluginCommand) {
	t.Helper()
	_, inv, free := prepareRuntimePlugin(t, c, target, op, "command")
	free()
	var p protocol.PluginCommand
	if err := json.Unmarshal(inv.Request.Params, &p); err != nil {
		t.Fatal(err)
	}
	return inv.Request, p
}

func pluginCommandCaller(t *testing.T, c *Core, agent, op, cwd string, legacy bool) *protocol.Caller {
	t.Helper()
	p := protocol.Submit{AgentID: agent, Input: "plugin command"}
	req, _ := protocol.NewRequest("agent.submit", "submit-"+op, p)
	a, err := c.Store.Submit(t.Context(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.ClaimNext(t.Context(), agent); err != nil {
		t.Fatal(err)
	}
	settings := taskSettings{Version: 2, Cwd: cwd}
	r := requestRecord{ID: "model-" + op, Settings: &settings}
	if legacy {
		r.Settings = nil
		if err := c.Store.AppendTaskEvent(t.Context(), agent, a.TaskID, "task.config", settings); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Store.AppendTaskEvent(t.Context(), agent, a.TaskID, "model.started", r); err != nil {
		t.Fatal(err)
	}
	call := invocation{ID: op, RequestID: r.ID, Call: model.ToolCall{ID: "call-" + op, Name: "exec_command", Arguments: json.RawMessage(`{"command":"zende plugin command"}`)}}
	if err := c.Store.AppendTaskEvent(t.Context(), agent, a.TaskID, "tool.dispatching", call); err != nil {
		t.Fatal(err)
	}
	return &protocol.Caller{AgentID: agent, TaskID: a.TaskID, OperationID: op}
}

func TestPluginCommandCallerKeepsRequestScopeAndOriginalReceipt(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "request-settings", true: "legacy-task-settings"}[legacy], func(t *testing.T) {
			c, target, _, _ := registeredPlugin(t, "call-cancel")
			req, input := pluginCommandRequest(t, c, target, "command")
			originalCwd := t.TempDir()
			input.Caller = pluginCommandCaller(t, c, target.AgentID, "shell", originalCwd, legacy)
			req.Params, _ = json.Marshal(input)
			// The Agent's current directory differs from the saved request.
			selection, err := c.Store.Selection(t.Context(), target.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			next := selection
			next.Choice.Cwd = t.TempDir()
			choose, _ := protocol.NewRequest("agent.select", "move", next.Choice)
			if _, err := c.Store.Select(t.Context(), choose, next, selection.Choice.Revision, false); err != nil {
				t.Fatal(err)
			}
			v, err := c.Handle(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			receipt := v.(store.PluginReceipt)
			if receipt.TaskID != input.Caller.TaskID || receipt.Source == nil || receipt.Source.Caller != *input.Caller || receipt.Source.RequestID != "model-shell" || receipt.Source.CallID != "call-shell" {
				t.Fatal(receipt)
			}
			record, err := c.Store.PluginCall(t.Context(), receipt.PluginCallTarget)
			if err != nil || record.Invocation.Scope.Cwd != originalCwd {
				t.Fatal(record, err)
			}
			waitPluginReceipt(t, c, receipt.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
			stop, _ := protocol.NewRequest("agent.stop", "stop", protocol.Target{AgentID: target.AgentID})
			if _, err := c.Handle(t.Context(), stop); err != nil {
				t.Fatal(err)
			}
			waitPluginReceipt(t, c, receipt.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "cancelled" })
			fresh := req
			fresh.OperationID = "new-after-stop"
			if _, err := c.Handle(t.Context(), fresh); !isFailure(err, "task_cancelled") {
				t.Fatal(err)
			}
			if err := c.Store.FinishTask(t.Context(), target.AgentID, input.Caller.TaskID, "cancelled", "", nil); err != nil {
				t.Fatal(err)
			}
			// Explicit retries may come from a new caller or a human. They retain
			// the first attribution even when its Task and instance are gone.
			c.CloseResources()
			for _, caller := range []*protocol.Caller{nil, {AgentID: target.AgentID, TaskID: "later-task", OperationID: "later-shell"}} {
				input.Caller = caller
				req.Params, _ = json.Marshal(input)
				v, err := c.Handle(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				got := v.(store.PluginReceipt)
				if got.Binding != receipt.Binding || got.Source == nil || *got.Source != *receipt.Source || got.TaskID != receipt.TaskID || got.State != "cancelled" {
					t.Fatal(got)
				}
			}
			input.Arguments = json.RawMessage(`{"text":"changed"}`)
			req.Params, _ = json.Marshal(input)
			if _, err := c.Handle(t.Context(), req); !isFailure(err, "operation_conflict") {
				t.Fatal(err)
			}
		})
	}
}

func TestPluginCommandRejectsUnboundOrClientAssignedScope(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	req, input := pluginCommandRequest(t, c, target, "command")
	for _, field := range []string{"task_id", "cwd"} {
		var params map[string]any
		json.Unmarshal(req.Params, &params)
		params[field] = "client-controlled"
		bad := req
		bad.Params, _ = json.Marshal(params)
		if _, err := c.Handle(t.Context(), bad); err == nil {
			t.Fatal("accepted", field)
		}
	}
	for _, caller := range []*protocol.Caller{{AgentID: "other", TaskID: "t", OperationID: "exec"}, {AgentID: target.AgentID, TaskID: "missing", OperationID: "exec"}} {
		input.Caller = caller
		req.Params, _ = json.Marshal(input)
		if _, err := c.Handle(t.Context(), req); err == nil {
			t.Fatal("accepted unbound caller")
		}
	}
	if prior, err := c.Store.PluginCall(t.Context(), protocol.PluginCallTarget{AgentID: target.AgentID, OperationID: req.OperationID}); err != nil || prior != nil {
		t.Fatal(prior, err)
	}
	input.Caller = nil
	req.Params, _ = json.Marshal(input)
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := v.(store.PluginReceipt)
	if r.Source != nil || r.TaskID != "" {
		t.Fatal(r)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
	stop, _ := protocol.NewRequest("agent.stop", "freeze", protocol.StopAgent{AgentID: target.AgentID, Tree: true})
	if _, err := c.Store.BeginTreeStop(t.Context(), stop, target.AgentID); err != nil {
		t.Fatal(err)
	}
	fresh := req
	fresh.OperationID = "after-freeze"
	if _, err := c.Handle(t.Context(), fresh); !isFailure(err, "tree_stopping") {
		t.Fatal("tree stop admitted new plugin work", err)
	}
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal("tree stop hid accepted receipt", err)
	}
}

func TestPluginCommandScopeRecheckedAtCommit(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	_, inv, free := prepareRuntimePlugin(t, c, target, "prepared", "command")
	defer free()
	selection, err := c.Store.Selection(t.Context(), target.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	next := selection
	next.Choice.Cwd = t.TempDir()
	req, _ := protocol.NewRequest("agent.select", "move", next.Choice)
	if _, err := c.Store.Select(t.Context(), req, next, selection.Choice.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := c.Store.BeginPluginCall(t.Context(), inv, c.InstanceID); fresh || !isFailure(err, "selection_changed") {
		t.Fatal(fresh, err)
	}
	if record, err := c.Store.PriorPluginCall(t.Context(), inv.Request); record != nil || err != nil {
		t.Fatal(record, err)
	}

	// The invocation's Scope cannot substitute a caller's request directory.
	caller := pluginCommandCaller(t, c, target.AgentID, "shell", selection.Choice.Cwd, false)
	var input store.PluginCallInput
	json.Unmarshal(inv.Request.Params, &input)
	input.Caller = caller
	inv.Request.Params, _ = json.Marshal(input)
	inv.Scope.TaskID, inv.Scope.Cwd = caller.TaskID, next.Choice.Cwd
	if _, fresh, err := c.Store.BeginPluginCall(t.Context(), inv, c.InstanceID); fresh || !isFailure(err, "invalid_state") {
		t.Fatal(fresh, err)
	}
	inv.Scope.Cwd = selection.Choice.Cwd
	if record, fresh, err := c.Store.BeginPluginCall(t.Context(), inv, c.InstanceID); err != nil || !fresh || record.Receipt.Source == nil || record.Invocation.Scope.Cwd != selection.Choice.Cwd {
		t.Fatal(record, fresh, err)
	}
}

func TestPluginCommandRetainsOpaqueProviderCallID(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "")
	req, input := pluginCommandRequest(t, c, target, "command")
	input.Caller = pluginCommandCaller(t, c, target.AgentID, "shell", t.TempDir(), false)
	input.Caller.OperationID = "opaque-shell"
	callID := strings.Repeat("opaque-", 70)
	dispatch := invocation{ID: input.Caller.OperationID, RequestID: "model-shell", Call: model.ToolCall{ID: callID, Name: "exec_command", Arguments: json.RawMessage(`{}`)}}
	if err := c.Store.AppendTaskEvent(t.Context(), target.AgentID, input.Caller.TaskID, "tool.dispatching", dispatch); err != nil {
		t.Fatal(err)
	}
	req.Params, _ = json.Marshal(input)
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := v.(store.PluginReceipt)
	if r.Source == nil || r.Source.CallID != callID {
		t.Fatal(r)
	}
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
}
