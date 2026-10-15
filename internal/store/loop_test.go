package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func TestClaimNextSerializesAndCommitsContext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "first"}
	req, _ := protocol.NewRequest("agent.create", "first", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	second := protocol.Submit{AgentID: a.AgentID, Input: "second"}
	req, _ = protocol.NewRequest("agent.submit", "second", second)
	b, err := s.Submit(ctx, req, second, 8)
	if err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := s.ClaimNext(ctx, a.AgentID)
			if err != nil {
				t.Error(err)
			}
			if w != nil {
				claims.Add(1)
				if w.Task.ID != a.TaskID {
					t.Error("claim skipped queue head")
				}
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal(claims.Load())
	}
	messages, _, err := s.ModelContext(ctx, a.AgentID)
	if err != nil || len(messages) != 1 || messages[0].Content[0].Text != "first" {
		t.Fatal(messages, err)
	}
	if err := s.FinishTask(ctx, a.AgentID, b.TaskID, "completed", "wrong", nil); err == nil {
		t.Fatal("unstarted task completed")
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "overwritten", nil); err == nil {
		t.Fatal("terminal result overwritten")
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil || w.Task.ID != b.TaskID {
		t.Fatal(w, err)
	}
	messages, _, err = s.ModelContext(ctx, a.AgentID)
	if err != nil || len(messages) != 2 {
		t.Fatal(messages, err)
	}
}

func TestInterruptedLoopIsHeldOnceWithoutReplaying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "keep context"}
	req, _ := protocol.NewRequest("agent.create", "initial", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "tool.started", map[string]string{"operation_id": "uncertain"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		if err := s.HoldInterrupted(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w != nil {
		t.Fatal("uncertain work replayed", w, err)
	}
	snap, err := s.Snapshot(ctx, a.AgentID)
	if err != nil || snap.Agent.State != "unknown" || snap.Tasks[0].State != "unknown" || snap.Tasks[0].Input != "keep context" {
		t.Fatal(snap, err)
	}
	page, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	unknown := 0
	for _, e := range page.Events {
		if e.Kind == "task.unknown" {
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatal(unknown)
	}
}

func TestResponseCommitDoesNotLeavePartialContext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "original"}
	req, _ := protocol.NewRequest("agent.create", "atomic-response", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	response := model.Response{Messages: []model.Message{
		{Role: "assistant", Content: []model.Block{{Type: "text", Text: "must roll back"}}},
		{Role: "assistant", Protocol: "fixture", Opaque: []byte("invalid JSON")},
	}}
	if err := s.RecordResponse(ctx, a.AgentID, a.TaskID, "request", response); err == nil {
		t.Fatal("invalid response committed")
	}
	after, err := s.Snapshot(ctx, a.AgentID)
	if err != nil || after.Cursor != before.Cursor {
		t.Fatal(after, err)
	}
	messages, _, err := s.ModelContext(ctx, a.AgentID)
	if err != nil || len(messages) != 1 || messages[0].Content[0].Text != "original" {
		t.Fatal(messages, err)
	}
}

func TestFormatRepairInstructionAndCounterCommitTogether(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "original"}
	req, _ := protocol.NewRequest("agent.create", "repair-atomic", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_repair_context BEFORE INSERT ON events WHEN NEW.kind='context.message' BEGIN SELECT RAISE(ABORT, 'test repair context failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFormatRepair(ctx, a.AgentID, a.TaskID, "request", 1, 1, "repair next response"); err == nil {
		t.Fatal("partial repair committed")
	}
	after, err := s.Snapshot(ctx, a.AgentID)
	if err != nil || after.Cursor != before.Cursor {
		t.Fatal(after, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_repair_context`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFormatRepair(ctx, a.AgentID, a.TaskID, "request", 1, 1, "repair next response"); err != nil {
		t.Fatal(err)
	}
	messages, _, err := s.ModelContext(ctx, a.AgentID)
	if err != nil || len(messages) != 2 || messages[0].Content[0].Text != "original" || messages[1].Role != "developer" {
		t.Fatal(messages, err)
	}
	if err := s.RecordFormatRepair(ctx, a.AgentID, a.TaskID, "request", 2, 1, "out of budget"); err == nil {
		t.Fatal("invalid attempt accepted")
	}
}
