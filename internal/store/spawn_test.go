package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func addSpawnContext(t *testing.T, s *Store, agent, task string, messages ...model.Message) {
	t.Helper()
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		for _, m := range messages {
			if _, err := appendEvent(context.Background(), tx, agent, task, "context.message", ContextMessage{RequestID: "parent-request", Message: m}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnAtomicInheritanceCallerAndPersistentReuse(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, parent := create(t, s)
	if _, err := s.ClaimNext(ctx, parent.AgentID); err != nil {
		t.Fatal(err)
	}
	_, imported := saveImage(t, s, parent.AgentID, "image")
	imageID := imported.Attachment.ID
	private := model.Message{Role: "assistant", Protocol: "responses", Opaque: json.RawMessage(`{"type":"reasoning","id":"reasoning","encrypted_content":"opaque-kept"}`)}
	addSpawnContext(t, s, parent.AgentID, parent.TaskID,
		model.Message{Role: "user", Content: []model.Block{{Type: "image", Attachment: imageID}, {Type: "image", Attachment: imageID}}},
		private,
		model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "finished", Name: "exec_command", Arguments: json.RawMessage(`{}`)}}},
		model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "finished", Text: "stdout"}, {Type: "text", CallID: "finished", Text: "stderr"}}},
		model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "pending", Name: "exec_command", Arguments: json.RawMessage(`{}`)}}},
		model.Message{Role: "assistant", Category: "commentary", Content: []model.Block{{Type: "text", Text: "pending parent comment"}}},
	)
	caller := &protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "host-call"}
	if err := s.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "model-request", "call": model.ToolCall{ID: "pending", Name: "exec_command", Arguments: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	selection, err := s.Selection(ctx, parent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	selection.Choice.Model, selection.Choice.Effort = "configured/model", "high"
	selection.Binding = json.RawMessage(`{"credential":"env:REFERENCE_ONLY","model":"test"}`)
	selectReq, _ := protocol.NewRequest("agent.select", "select", protocol.SelectAgent{AgentID: parent.AgentID, Model: &selection.Choice.Model})
	selected, err := s.Select(ctx, selectReq, selection, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.ModelContext(ctx, parent.AgentID)
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "independent task", Caller: caller}
	req, _ := protocol.NewRequest("agent.spawn", "spawn-once", p)
	result, err := s.Spawn(ctx, req, p, selection.Choice.Cwd, selected.Selection.Revision, 8, 4)
	if err != nil {
		t.Fatal(err)
	}
	if result.Spawn == nil || result.Spawn.Messages != 5 || result.Spawn.ExcludedPending != 2 || result.Spawn.CallID != "pending" || result.Spawn.RequestID != "model-request" || result.AgentID == parent.AgentID || result.TaskID == "" {
		t.Fatalf("%+v source=%+v", result, result.Spawn)
	}
	child, err := s.Snapshot(ctx, result.AgentID)
	if err != nil || child.Agent.ParentID != parent.AgentID || child.Agent.Cwd != selection.Choice.Cwd || child.Agent.State != "idle" || len(child.Tasks) != 1 || child.Tasks[0].State != "accepted" {
		t.Fatal(child, err)
	}
	saved, err := s.Selection(ctx, result.AgentID)
	if err != nil || saved.Choice.ModelRevision != 2 || saved.Choice.Effort != "high" || !bytes.Equal(saved.Binding, selection.Binding) {
		t.Fatal(saved, err)
	}
	messages, _, err := s.ModelContext(ctx, result.AgentID)
	if err != nil || len(messages) != 6 || !reflect.DeepEqual(messages[2], private) || messages[5].Role != "developer" {
		t.Fatal(messages, err)
	}
	if messages[1].Content[0].Attachment != result.Spawn.Images[imageID] || messages[1].Content[0].Attachment == imageID || messages[1].Content[1].Attachment != messages[1].Content[0].Attachment {
		t.Fatal("image mapping lost")
	}
	image, err := s.ReadImage(ctx, protocol.AttachmentTarget{AgentID: result.AgentID, ID: result.Spawn.Images[imageID]})
	if err != nil || !bytes.Equal(image.Data, imageBytes(t)) {
		t.Fatal("child image unavailable", err)
	}
	after, _, _ := s.ModelContext(ctx, parent.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("spawn changed parent context")
	}
	var result2 protocol.Accepted
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			again, err := s.Spawn(ctx, req, p, "/no-longer-valid", -1, 0, 0)
			if err != nil || !reflect.DeepEqual(result, again) {
				t.Error("retry changed identity", again, err)
			}
		})
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result2, err = reopened.Spawn(ctx, req, p, "/gone", -1, 0, 0)
	if err != nil || !reflect.DeepEqual(result, result2) {
		t.Fatal("receipt lost after restart", result2, err)
	}
	agents, _ := reopened.ListAgents(ctx)
	if len(agents) != 2 {
		t.Fatal("duplicate child", agents)
	}
	if w, err := reopened.ClaimNext(ctx, result.AgentID); err != nil || w == nil || w.Task.ID != result.TaskID {
		t.Fatal(w, err)
	}
	if err := reopened.FinishTask(ctx, result.AgentID, result.TaskID, "completed", "first done", nil); err != nil {
		t.Fatal(err)
	}
	next := protocol.Submit{AgentID: result.AgentID, Input: "follow-up"}
	nextReq, _ := protocol.NewRequest("agent.submit", "follow-up", next)
	accepted, err := reopened.Submit(ctx, nextReq, next, 8)
	if err != nil || accepted.AgentID != result.AgentID || accepted.TaskID == result.TaskID {
		t.Fatal(accepted, err)
	}
	if w, err := reopened.ClaimNext(ctx, result.AgentID); err != nil || w == nil || w.Task.ID != accepted.TaskID {
		t.Fatal(w, err)
	}
	messages, _, _ = reopened.ModelContext(ctx, result.AgentID)
	if len(messages) != 8 || messages[7].Content[0].Text != "follow-up" {
		t.Fatal("reuse lost history", messages)
	}
	req.Params, _ = json.Marshal(protocol.SpawnAgent{ParentID: parent.AgentID, Input: "different"})
	if _, err := reopened.Spawn(ctx, req, p, "/", 0, 8, 4); err == nil {
		t.Fatal("conflicting operation accepted")
	}
}

func TestSpawnRejectsWithoutPartialIdentity(t *testing.T) {
	for _, variant := range []string{"capacity", "depth", "revision", "caller-missing", "caller-parent", "caller-tool", "context", "empty", "archived", "copying", "missing-image", "orphan-result", "rollback-child", "rollback-parent", "rollback-receipt"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, parent := create(t, s)
			selection, _ := s.Selection(ctx, parent.AgentID)
			p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "child"}
			maxAgents, maxDepth, revision := 8, 4, int64(0)
			switch variant {
			case "capacity":
				maxAgents = 1
			case "depth":
				maxDepth = 0
			case "revision":
				revision = 1
			case "caller-missing":
				p.Caller = &protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "not-dispatched"}
			case "caller-parent":
				p.Caller = &protocol.Caller{AgentID: "other", TaskID: parent.TaskID, OperationID: "call"}
			case "caller-tool":
				_, _ = s.ClaimNext(ctx, parent.AgentID)
				p.Caller = &protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "not-exec"}
				if err := s.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", map[string]any{"operation_id": "not-exec", "request_id": "r", "call": model.ToolCall{ID: "c", Name: "apply_patch"}}); err != nil {
					t.Fatal(err)
				}
			case "context":
				p.Context = "unsupported"
			case "empty":
				p.Input = " "
			case "archived", "copying":
				if _, err := s.db.Exec("UPDATE agents SET state=? WHERE id=?", variant, parent.AgentID); err != nil {
					t.Fatal(err)
				}
			case "missing-image":
				addSpawnContext(t, s, parent.AgentID, "", model.Message{Role: "user", Content: []model.Block{{Type: "image", Attachment: "missing"}}})
			case "orphan-result":
				addSpawnContext(t, s, parent.AgentID, "", model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "missing"}}})
			case "rollback-child", "rollback-parent":
				kind := "context.inherited"
				if variant == "rollback-parent" {
					kind = "agent.spawned"
				}
				if _, err := s.db.Exec("CREATE TRIGGER fail_spawn BEFORE INSERT ON events WHEN NEW.kind='" + kind + "' BEGIN SELECT RAISE(ABORT,'test rollback'); END;"); err != nil {
					t.Fatal(err)
				}
			case "rollback-receipt":
				if _, err := s.db.Exec("CREATE TRIGGER fail_spawn BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'test rollback'); END;"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.Events(ctx, protocol.Cursor{AgentID: parent.AgentID})
			req, _ := protocol.NewRequest("agent.spawn", variant, p)
			accepted, err := s.Spawn(ctx, req, p, selection.Choice.Cwd, revision, maxAgents, maxDepth)
			if err == nil || accepted.AgentID != "" || accepted.TaskID != "" {
				t.Fatal("failed spawn exposed identity", accepted, err)
			}
			after, _ := s.Events(ctx, protocol.Cursor{AgentID: parent.AgentID})
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed spawn changed parent")
			}
			agents, _ := s.ListAgents(ctx)
			if len(agents) != 1 {
				t.Fatal("partial child", agents)
			}
			if _, err := s.Operation(ctx, req.OperationID); err == nil {
				t.Fatal("partial receipt")
			}
		})
	}
}

func TestSpawnNoneAndDepth(t *testing.T) {
	s, _ := newStore(t)
	_, parent := create(t, s)
	ctx := context.Background()
	// none need not inspect or repair malformed/unsettled parent context.
	addSpawnContext(t, s, parent.AgentID, "", model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "missing"}}})
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "new task", Context: "none"}
	req, _ := protocol.NewRequest("agent.spawn", "none", p)
	selection, _ := s.Selection(ctx, parent.AgentID)
	child, err := s.Spawn(ctx, req, p, selection.Choice.Cwd, 0, 8, 1)
	if err != nil || child.Spawn.Messages != 0 || child.Spawn.First != 0 || child.Spawn.Last != 0 {
		t.Fatal(child, err)
	}
	messages, _, _ := s.ModelContext(ctx, child.AgentID)
	if len(messages) != 1 || messages[0].Role != "developer" {
		t.Fatal(messages)
	}
	p.ParentID = child.AgentID
	req, _ = protocol.NewRequest("agent.spawn", "too-deep", p)
	if _, err := s.Spawn(ctx, req, p, selection.Choice.Cwd, 2, 8, 1); err == nil {
		t.Fatal("depth limit not enforced")
	}
}

func TestConcurrentSpawnAcceptanceCreatesOneChild(t *testing.T) {
	s, _ := newStore(t)
	_, parent := create(t, s)
	ctx := context.Background()
	selection, _ := s.Selection(ctx, parent.AgentID)
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "child"}
	req, _ := protocol.NewRequest("agent.spawn", "concurrent", p)
	results := make(chan protocol.Accepted, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			accepted, err := s.Spawn(ctx, req, p, selection.Choice.Cwd, 0, 8, 4)
			if err != nil {
				t.Error(err)
				return
			}
			results <- accepted
		})
	}
	wg.Wait()
	close(results)
	first := <-results
	if first.AgentID == "" {
		t.Fatal("no accepted child")
	}
	for result := range results {
		if !reflect.DeepEqual(result, first) {
			t.Fatal("different child", result)
		}
	}
	agents, _ := s.ListAgents(ctx)
	if len(agents) != 2 {
		t.Fatal(agents)
	}
	page, _ := s.Events(ctx, protocol.Cursor{AgentID: parent.AgentID})
	count := 0
	for _, event := range page.Events {
		if event.Kind == "agent.spawned" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate spawn event", count)
	}
}

func TestInheritedContextDoesNotSplitParallelToolGroup(t *testing.T) {
	text := func(value string) contextEntry {
		return contextEntry{Record: ContextMessage{Message: model.Message{Role: "user", Content: []model.Block{{Type: "text", Text: value}}}}}
	}
	call := func(id string) contextEntry {
		return contextEntry{Record: ContextMessage{Message: model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: id}}}}}
	}
	result := func(id string) contextEntry {
		return contextEntry{Record: ContextMessage{Message: model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: id}}}}}
	}
	entries := []contextEntry{text("history"), call("one"), call("two"), result("one")}
	if prefix, err := completePrefix(entries); err != nil || prefix != 1 {
		t.Fatal("split parallel group", prefix, err)
	}
	entries = append(entries, result("two"), text("next"))
	if prefix, err := completePrefix(entries); err != nil || prefix != len(entries) {
		t.Fatal("complete group excluded", prefix, err)
	}
	if _, err := completePrefix(append(entries, call("one"))); err == nil {
		t.Fatal("duplicate historical call accepted")
	}
}
