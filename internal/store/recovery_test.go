package store

import (
	"reflect"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func TestRecoveryAcceptanceAndActivationPreserveOriginalTask(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	held, err := s.HeldTasks(ctx)
	if err != nil || len(held) != 1 || held[0].TaskID != a.TaskID || held[0].RequestSeq != 0 {
		t.Fatal(held, err)
	}
	p := protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID}
	req, _ := protocol.NewRequest("agent.recover", "recover-once", p)
	first, err := s.RequestRecovery(ctx, req, p)
	if err != nil || first.TaskID != a.TaskID {
		t.Fatal(first, err)
	}
	before, _ := s.Snapshot(ctx, a.AgentID)
	for range 2 {
		again, err := s.RequestRecovery(ctx, req, p)
		if err != nil || again != first {
			t.Fatal(again, err)
		}
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed history")
	}
	held, _ = s.HeldTasks(ctx)
	requested := held[0].RequestSeq
	enqueue(t, s, ctx, a.AgentID, "not proof")
	held, _ = s.HeldTasks(ctx)
	if held[0].RequestSeq != requested {
		t.Fatal("input authorized verification retry")
	}
	if _, err := s.ActivateHeld(ctx, a.AgentID, a.TaskID, before.Cursor); err == nil {
		t.Fatal("stale verification activated")
	}
	current, _ := s.Snapshot(ctx, a.AgentID)
	w, err := s.ActivateHeld(ctx, a.AgentID, a.TaskID, current.Cursor)
	if err != nil || w.Task.ID != a.TaskID || w.Task.State != "running" {
		t.Fatal(w, err)
	}
	if _, err := s.ActivateHeld(ctx, a.AgentID, a.TaskID, w.Agent.Seq); err == nil {
		t.Fatal("second activation accepted")
	}
	messages, _, _ := s.ModelContext(ctx, a.AgentID)
	if len(messages) != 1 {
		t.Fatal("input duplicated or queue consumed", messages)
	}
}

func TestVerifiedResultAndActivationRollback(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	s.ClaimNext(ctx, a.AgentID)
	s.HoldInterrupted(ctx)
	before, _ := s.Snapshot(ctx, a.AgentID)
	result := VerifiedResult{RequestID: "request", OperationID: "operation", Outcome: map[string]string{"saved": "effect"}, Message: model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "call", Text: "saved effect"}}}}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_verified_message BEFORE INSERT ON events WHEN NEW.kind='context.message' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVerifiedResults(ctx, a.AgentID, a.TaskID, before.Cursor, []VerifiedResult{result}); err == nil {
		t.Fatal("injection failed")
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial proof committed")
	}
	s.db.Exec("DROP TRIGGER fail_verified_message")
	if err := s.RecordVerifiedResults(ctx, a.AgentID, a.TaskID, before.Cursor, []VerifiedResult{result}); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Snapshot(ctx, a.AgentID)
	if after.Agent.State != "unknown" || after.Cursor != before.Cursor+3 {
		t.Fatal(after)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_recovery BEFORE INSERT ON events WHEN NEW.kind='task.recovered' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateHeld(ctx, a.AgentID, a.TaskID, after.Cursor); err == nil {
		t.Fatal("injection failed")
	}
	current, _ := s.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(current, after) {
		t.Fatal("partial activation committed")
	}
}
