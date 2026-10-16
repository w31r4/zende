package runtime

import (
	"path/filepath"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

func TestPluginCommandTreeStopOwnsOnlyCapturedAgents(t *testing.T) {
	c, root, _, source := registeredPlugin(t, "call-cancel")
	_, runner, _ := remoteRunner(t)
	c.Runner = runner
	child := treeChild(t, c, root.AgentID)
	create := protocol.CreateAgent{Cwd: filepath.Dir(source)}
	createReq, _ := protocol.NewRequest("agent.create", "other-agent", create)
	other, err := c.Store.Create(t.Context(), createReq, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	targets := []protocol.ResourceTarget{root}
	for _, a := range []protocol.Accepted{child, other} {
		req, _ := protocol.NewRequest("resource.register", "register-"+a.AgentID, protocol.RegisterResource{AgentID: a.AgentID, Path: source, Kind: "plugin"})
		v, err := c.Handle(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, protocol.ResourceTarget{AgentID: a.AgentID, ID: v.(protocol.Accepted).Resource.ID})
	}
	var calls []store.PluginReceipt
	for _, target := range targets {
		req, _ := pluginCommandRequest(t, c, target, "command-"+target.AgentID)
		v, err := c.Handle(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		r := v.(store.PluginReceipt)
		if r.TaskID != "" {
			t.Fatal("human command acquired a Task", r)
		}
		waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.ProgressSeq == 1 })
		calls = append(calls, r)
	}
	// Ordinary stop pauses the Agent's Task, preserving independent commands.
	stop, _ := protocol.NewRequest("agent.stop", "ordinary", protocol.Target{AgentID: root.AgentID})
	if _, err := c.Handle(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	r, err := c.Store.PluginCall(t.Context(), calls[0].PluginCallTarget)
	if err != nil || r.Receipt.State != "dispatched" {
		t.Fatal(r, err)
	}

	tree, _ := protocol.NewRequest("agent.stop", "tree", protocol.StopAgent{AgentID: root.AgentID, Tree: true})
	pluginTestSQL(t, c, `CREATE TRIGGER fail_tree_accept BEFORE INSERT ON operations WHEN NEW.id='tree' BEGIN SELECT RAISE(ABORT,'injected rollback'); END`)
	if _, err := c.Handle(t.Context(), tree); err == nil {
		t.Fatal("accepted failed tree transaction")
	}
	for _, call := range calls {
		r, err := c.Store.PluginCall(t.Context(), call.PluginCallTarget)
		if err != nil || r.Receipt.State != "dispatched" {
			t.Fatal("cancel escaped rollback", r, err)
		}
	}
	pluginTestSQL(t, c, `DROP TRIGGER fail_tree_accept`)
	// Commit without signaling first, so readiness must observe pending calls
	// even though the root has no active Task.
	if _, err := c.Store.BeginTreeStop(t.Context(), tree, root.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.TreeStopReady(t.Context(), "tree"); !isFailure(err, "tree_stopping") {
		t.Fatal(err)
	}
	for _, call := range calls[:2] {
		r, err := c.Store.PluginCall(t.Context(), call.PluginCallTarget)
		if err != nil || r.Receipt.State != "cancelling" {
			t.Fatal(r, err)
		}
	}
	if _, err := c.advanceTreeStop(t.Context(), "tree"); err != nil && !isFailure(err, "tree_stopping") {
		t.Fatal(err)
	}
	for _, call := range calls[:2] {
		waitPluginReceipt(t, c, call.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "cancelled" })
	}
	if stage, err := c.advanceTreeStop(t.Context(), "tree"); err != nil || stage != "" {
		t.Fatal(stage, err)
	}
	plan, err := c.Store.TreeStop(t.Context(), "tree")
	if err != nil || plan.State != "stopped" {
		t.Fatal(plan, err)
	}
	r, err = c.Store.PluginCall(t.Context(), calls[2].PluginCallTarget)
	if err != nil || r.Receipt.State != "dispatched" {
		t.Fatal("unrelated Agent was cancelled", r, err)
	}
}

func TestPluginCommandUnknownPreventsTreeStopCompletion(t *testing.T) {
	c, target, _, _ := registeredPlugin(t, "call-exit")
	_, runner, _ := remoteRunner(t)
	c.Runner = runner
	req, _ := pluginCommandRequest(t, c, target, "unknown")
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := v.(store.PluginReceipt)
	waitPluginReceipt(t, c, r.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "unknown" })
	stop := requestTreeStop(t, c, target.AgentID, "tree")
	if _, err := c.advanceTreeStop(t.Context(), stop.OperationID); !isFailure(err, "tree_stopping") {
		t.Fatal("unknown plugin reported stopped", err)
	}
	plan, err := c.Store.TreeStop(t.Context(), stop.OperationID)
	if err != nil || plan.State == "stopped" {
		t.Fatal(plan, err)
	}
}
