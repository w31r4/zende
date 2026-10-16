package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func preparedSelection(t *testing.T, s *Store, agent, op string) (protocol.Request, Selection) {
	t.Helper()
	next, err := s.Selection(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	next.Choice.Cwd, next.Choice.Model, next.Choice.Effort = t.TempDir(), "local/model", "high"
	next.Binding = json.RawMessage(`{"version":1,"credential":"env:UNRESOLVED_TEST_KEY"}`)
	p := protocol.SelectAgent{AgentID: agent, Cwd: &next.Choice.Cwd, Model: &next.Choice.Model, Effort: &next.Choice.Effort}
	req, _ := protocol.NewRequest("agent.select", op, p)
	return req, next
}

func TestSelectionDurableAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, a := create(t, s)
	old, _ := s.Snapshot(ctx, a.AgentID)
	history, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	req, next := preparedSelection(t, s, a.AgentID, "selection")
	accepted, err := s.Select(ctx, req, next, 0, true)
	if err != nil || accepted.Selection == nil || accepted.Selection.Revision != old.Cursor+1 || accepted.Selection.ModelRevision != accepted.Selection.Revision {
		t.Fatal(accepted, err)
	}
	// A retry with stale preparation returns the original receipt, not another change.
	again, err := s.Select(ctx, req, Selection{}, -1, false)
	if err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal(again, err)
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	events, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if after.Agent.State != old.Agent.State || !reflect.DeepEqual(after.Tasks, old.Tasks) || len(events.Events) != len(history.Events)+1 || !reflect.DeepEqual(events.Events[:len(history.Events)], history.Events) {
		t.Fatal("selection changed work or rewrote history", after)
	}
	req, cwdOnly := preparedSelection(t, s, a.AgentID, "cwd-only")
	cwdOnly.Choice.Model, cwdOnly.Choice.Effort, cwdOnly.Binding = "ignored", "ignored", nil
	selected, err := s.Select(ctx, req, cwdOnly, accepted.Selection.Revision, false)
	if err != nil || selected.Selection.Model != accepted.Selection.Model || selected.Selection.ModelRevision != accepted.Selection.ModelRevision || selected.Selection.Effort != "high" {
		t.Fatal(selected, err)
	}
	stale, _ := protocol.NewRequest("agent.select", "stale", protocol.Target{AgentID: a.AgentID})
	if _, err := s.Select(ctx, stale, next, accepted.Selection.Revision, true); err == nil {
		t.Fatal("stale selection replaced a concurrent change")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.Selection(ctx, a.AgentID)
	if err != nil || saved.Choice != *selected.Selection || !reflect.DeepEqual(saved.Binding, next.Binding) {
		t.Fatal(saved, err)
	}
}

func TestSelectionRollbackAndHeldStates(t *testing.T) {
	for _, state := range []string{"idle", "unknown", "paused", "copying", "archived"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, a := create(t, s)
			if _, err := s.db.Exec("UPDATE agents SET state=?,paused=? WHERE id=?", state, state == "paused", a.AgentID); err != nil {
				t.Fatal(err)
			}
			req, next := preparedSelection(t, s, a.AgentID, "select")
			if state == "idle" {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_selection BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.Snapshot(ctx, a.AgentID)
			_, err := s.Select(ctx, req, next, 0, true)
			after, _ := s.Snapshot(ctx, a.AgentID)
			rejected := state == "idle" || state == "copying" || state == "archived"
			if rejected {
				if err == nil || !reflect.DeepEqual(before, after) {
					t.Fatal("rejected selection leaked", err, after)
				}
				choice, _ := s.Selection(ctx, a.AgentID)
				if choice.Choice.Revision != 0 || len(choice.Binding) != 0 {
					t.Fatal(choice)
				}
			} else if err != nil || after.Agent.State != before.Agent.State || after.Agent.Paused != before.Agent.Paused || !reflect.DeepEqual(before.Tasks, after.Tasks) {
				t.Fatal("selection resumed held work", err, after)
			}
		})
	}
}

func TestRequestBoundaryFencesSelectionContextAndStop(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	_, cursor, _ := s.ModelContext(ctx, a.AgentID)
	req, next := preparedSelection(t, s, a.AgentID, "select")
	accepted, err := s.Select(ctx, req, next, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	record := func(cursor int64) any { return map[string]any{"context_cursor": cursor} }
	notices := func(text string) []ContextMessage {
		return []ContextMessage{{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: text}}}}}
	}
	// Neither a stale selection nor an unrelated new event may slip through.
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, 0, cursor, notices("must not append"), record); err == nil {
		t.Fatal("stale request started")
	}
	_, cursor, _ = s.ModelContext(ctx, a.AgentID)
	if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "test.concurrent", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, accepted.Selection.Revision, cursor, notices("must not append"), record); err == nil {
		t.Fatal("stale context started")
	}
	_, cursor, _ = s.ModelContext(ctx, a.AgentID)
	before, _ := s.Snapshot(ctx, a.AgentID)
	// Invalid record encoding rolls back the already inserted notice.
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, accepted.Selection.Revision, cursor, notices("rolled back"), func(int64) any { return make(chan int) }); err == nil {
		t.Fatal("invalid request persisted")
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("notice escaped failed transaction")
	}
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, accepted.Selection.Revision, cursor, notices("current settings"), record); err != nil {
		t.Fatal(err)
	}
	messages, _, _ := s.ModelContext(ctx, a.AgentID)
	last := messages[len(messages)-1]
	if last.Role != "developer" || last.Content[0].Text != "current settings" {
		t.Fatal(messages)
	}
	events, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, After: cursor})
	if len(events.Events) != 2 || events.Events[0].Kind != "context.message" || events.Events[1].Kind != "model.started" {
		t.Fatal(events)
	}
	var saved struct {
		Cursor int64 `json:"context_cursor"`
	}
	if json.Unmarshal(events.Events[1].Payload, &saved) != nil || saved.Cursor != events.Events[0].Seq {
		t.Fatal(saved)
	}
	if _, err := s.Stop(ctx, stopRequest(a.AgentID, "stop"), protocol.Target{AgentID: a.AgentID}); err != nil {
		t.Fatal(err)
	}
	_, cursor, _ = s.ModelContext(ctx, a.AgentID)
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, accepted.Selection.Revision, cursor, nil, record); err == nil {
		t.Fatal("stopped task dispatched")
	}
}

func TestSchemaSixMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, a := create(t, s)
	original, _ := s.Snapshot(ctx, a.AgentID)
	messages, _, _ := s.ModelContext(ctx, a.AgentID)
	history, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if _, err := s.db.Exec(dropPluginSubscriptionSchema + "DROP TABLE plugin_progress; DROP TABLE plugin_calls; DROP TABLE mcp_calls; DROP TABLE resource_updates; DROP TABLE wait_receipts; DROP TABLE mailbox; DROP TABLE workspace_operations; DROP TABLE agent_selections; PRAGMA user_version=6;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, _ := reopened.Snapshot(ctx, a.AgentID)
	saved, _, _ := reopened.ModelContext(ctx, a.AgentID)
	events, _ := reopened.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	choice, err := reopened.Selection(ctx, a.AgentID)
	if err != nil || !reflect.DeepEqual(original, after) || !reflect.DeepEqual(messages, saved) || !reflect.DeepEqual(events, history) || choice.Choice.Revision != 0 || choice.Choice.Cwd != original.Agent.Cwd {
		t.Fatal("migration changed data", choice, err)
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	// Version-six skill projections still exist; this migration only adds selection.
	if _, err := reopened.db.Exec("SELECT skills FROM tasks LIMIT 1"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSelectionHasOneWinner(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	requests := make([]protocol.Request, 8)
	choices := make([]Selection, len(requests))
	for i := range requests {
		requests[i], choices[i] = preparedSelection(t, s, a.AgentID, protocol.NewID("select"))
	}
	var wg sync.WaitGroup
	results := make(chan error, len(requests))
	for i := range requests {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Select(ctx, requests[i], choices[i], 0, true); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	page, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil || success != 1 || len(page.Events) != 3 {
		t.Fatal(success, page, err)
	}
}

func TestQuestionImagesUseLastDispatchedSelection(t *testing.T) {
	for _, images := range []bool{false, true} {
		t.Run(fmt.Sprint(images), func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			w := questionWork(t, s, !images)
			// Latest request capabilities supersede the initial Task snapshot.
			if err := s.AppendTaskEvent(ctx, w.Agent.ID, w.Task.ID, "model.started", map[string]any{"settings": map[string]bool{"images": images}}); err != nil {
				t.Fatal(err)
			}
			_, asked := askSaved(t, s, w, "image answer")
			_, saved := saveImage(t, s, w.Agent.ID, "image")
			p := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Attachments: []string{saved.Attachment.ID}}
			req, _ := protocol.NewRequest("question.answer", "answer", p)
			_, err := s.AnswerQuestion(ctx, req, p)
			if (err == nil) != images {
				t.Fatal("used initial rather than latest request capabilities", images, err)
			}
		})
	}
}
