package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func controlWork(t *testing.T, s *Store) (context.Context, protocol.Accepted) {
	t.Helper()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "first"}
	r, _ := protocol.NewRequest("agent.create", protocol.NewID("op"), p)
	a, err := s.Create(ctx, r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, a
}
func stopRequest(id, op string) protocol.Request {
	r, _ := protocol.NewRequest("agent.stop", op, protocol.Target{AgentID: id})
	return r
}
func resumeRequest(id, op string) protocol.Request {
	r, _ := protocol.NewRequest("agent.resume", op, protocol.Target{AgentID: id})
	return r
}
func enqueue(t *testing.T, s *Store, ctx context.Context, id, text string) protocol.Accepted {
	t.Helper()
	p := protocol.Submit{AgentID: id, Input: text}
	r, _ := protocol.NewRequest("agent.submit", protocol.NewID("op"), p)
	a, err := s.Submit(ctx, r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStopResumePreserveTerminalHistoryAndDedup(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	q := enqueue(t, s, ctx, a.AgentID, "queued")
	r := stopRequest(a.AgentID, "stop-once")
	target := protocol.Target{AgentID: a.AgentID}
	accepted, err := s.Stop(ctx, r, target)
	if err != nil || accepted.TaskID != a.TaskID {
		t.Fatal(accepted, err)
	}
	before, _ := s.Snapshot(ctx, a.AgentID)
	if before.Agent.State != "cancelling" || !before.Agent.Paused || before.Tasks[0].State != "cancelling" {
		t.Fatal(before)
	}
	for range 2 {
		again, err := s.Stop(ctx, r, target)
		if err != nil || again != accepted {
			t.Fatal(again, err)
		}
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	if after.Cursor != before.Cursor {
		t.Fatal("retry changed history")
	}
	for _, kind := range []string{"tool.started", "model.started"} {
		if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, kind, struct{}{}); err == nil {
			t.Fatal("new dispatch after stop", kind)
		}
	}
	if err := s.RecordToolResult(ctx, a.AgentID, a.TaskID, "request", map[string]string{"result": "saved effect"}, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "call", Text: "effect remains"}}}); err != nil {
		t.Fatal("result could not settle", err)
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "late final", nil); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Snapshot(ctx, a.AgentID)
	if after.Agent.State != "paused" || after.Tasks[0].State != "cancelled" || after.Tasks[0].Result != "" {
		t.Fatal(after)
	}
	if w, err := s.ClaimNext(ctx, a.AgentID); err != nil || w != nil {
		t.Fatal("pause bypassed", w, err)
	}
	resReq := resumeRequest(a.AgentID, "resume-once")
	res, err := s.Resume(ctx, resReq, target, 8)
	if err != nil || res.TaskID != q.TaskID {
		t.Fatal(res, err)
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil || w.Task.ID != q.TaskID {
		t.Fatal(w, err)
	}
	if again, err := s.Stop(ctx, r, target); err != nil || again != accepted {
		t.Fatal(again, err)
	}
	after, _ = s.Snapshot(ctx, a.AgentID)
	if after.Agent.State != "running" || after.Agent.Paused || after.Tasks[0].State != "cancelled" {
		t.Fatal("old stop targeted new task", after)
	}
	if again, err := s.Resume(ctx, resReq, target, 8); err != nil || again != res {
		t.Fatal(again, err)
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "revived", nil); err == nil {
		t.Fatal("cancelled task revived")
	}
}

func TestExplicitInputPrecedesQueueAndResumeDuringCancellationWaits(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	s.ClaimNext(ctx, a.AgentID)
	queued := enqueue(t, s, ctx, a.AgentID, "old queue")
	if _, err := s.Stop(ctx, stopRequest(a.AgentID, "stop"), protocol.Target{AgentID: a.AgentID}); err != nil {
		t.Fatal(err)
	}
	newTask := enqueue(t, s, ctx, a.AgentID, "new explicit input")
	snap, _ := s.Snapshot(ctx, a.AgentID)
	if snap.Agent.Paused || snap.Agent.State != "cancelling" {
		t.Fatal(snap)
	}
	if w, err := s.ClaimNext(ctx, a.AgentID); err != nil || w != nil {
		t.Fatal("second loop during cancellation", w, err)
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "failed", "", nil); err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil || w.Task.ID != newTask.TaskID {
		t.Fatal("explicit input not first", w, err)
	}
	if err := s.FinishTask(ctx, a.AgentID, w.Task.ID, "completed", "new done", nil); err != nil {
		t.Fatal(err)
	}
	w, err = s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil || w.Task.ID != queued.TaskID {
		t.Fatal(w, err)
	}
}

func TestUnknownCancellationCannotBeResumedOrBypassed(t *testing.T) {
	s, path := newStore(t)
	ctx, a := controlWork(t, s)
	s.ClaimNext(ctx, a.AgentID)
	target := protocol.Target{AgentID: a.AgentID}
	if _, err := s.Stop(ctx, stopRequest(a.AgentID, "stop"), target); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "unknown", "", protocol.E("operation_unknown", "effect unknown")); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, ctx, a.AgentID, "new ID cannot bypass")
	if _, err := s.Resume(ctx, resumeRequest(a.AgentID, "resume"), target, 8); err == nil {
		t.Fatal("unknown resumed")
	}
	snap, _ := s.Snapshot(ctx, a.AgentID)
	if snap.Agent.State != "unknown" || !snap.Agent.Paused || snap.Tasks[0].State != "cancelling" {
		t.Fatal(snap)
	}
	if w, err := s.ClaimNext(ctx, a.AgentID); err != nil || w != nil {
		t.Fatal(w, err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.HoldInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := reopened.Snapshot(ctx, a.AgentID)
	if after.Cursor != snap.Cursor || after.Agent != snap.Agent {
		t.Fatal(after, snap)
	}
}

func TestIdlePauseResumeCreatesNewTaskAndUnknownCrashKeepsPause(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	s.ClaimNext(ctx, a.AgentID)
	s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "old done", nil)
	target := protocol.Target{AgentID: a.AgentID}
	stop, err := s.Stop(ctx, stopRequest(a.AgentID, "idle-stop"), target)
	if err != nil || stop.TaskID != "" {
		t.Fatal(stop, err)
	}
	res, err := s.Resume(ctx, resumeRequest(a.AgentID, "idle-resume"), target, 8)
	if err != nil || res.TaskID == "" || res.TaskID == a.TaskID {
		t.Fatal(res, err)
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil || w.Task.ID != res.TaskID {
		t.Fatal(w, err)
	}
	s.Stop(ctx, stopRequest(a.AgentID, "active-stop"), target)
	for range 2 {
		if err := s.HoldInterrupted(ctx); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := s.Snapshot(ctx, a.AgentID)
	if snap.Agent.State != "unknown" || !snap.Agent.Paused || snap.Tasks[0].State != "completed" || snap.Tasks[1].State != "cancelling" {
		t.Fatal(snap)
	}
}

func TestStopAndFinalRaceHasOneTerminalAndPersistentPause(t *testing.T) {
	s, _ := newStore(t)
	for range 30 {
		ctx, a := controlWork(t, s)
		s.ClaimNext(ctx, a.AgentID)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.Stop(ctx, stopRequest(a.AgentID, protocol.NewID("stop")), protocol.Target{AgentID: a.AgentID}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "final", nil); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		snap, err := s.Snapshot(ctx, a.AgentID)
		if err != nil || !snap.Agent.Paused || snap.Agent.State != "paused" || (snap.Tasks[0].State != "completed" && snap.Tasks[0].State != "cancelled") {
			t.Fatal(snap, err)
		}
		page, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		terminal := 0
		for _, e := range page.Events {
			if e.Kind == "task.completed" || e.Kind == "task.cancelled" {
				terminal++
			}
		}
		if terminal != 1 {
			t.Fatal(terminal)
		}
	}
}

func TestStopAndResumeTransactionsRollBackCompletely(t *testing.T) {
	s, _ := newStore(t)
	ctx, a := controlWork(t, s)
	s.ClaimNext(ctx, a.AgentID)
	target := protocol.Target{AgentID: a.AgentID}
	before, _ := s.Snapshot(ctx, a.AgentID)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pause BEFORE INSERT ON events WHEN NEW.kind='agent.paused' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(ctx, stopRequest(a.AgentID, "failed-stop"), target); err == nil {
		t.Fatal("partial stop accepted")
	}
	after, _ := s.Snapshot(ctx, a.AgentID)
	aJSON, _ := json.Marshal(after)
	bJSON, _ := json.Marshal(before)
	if string(aJSON) != string(bJSON) {
		t.Fatal(after, before)
	}
	if _, err := s.Operation(ctx, "failed-stop"); err == nil {
		t.Fatal("failed stop retained acceptance")
	}
	s.db.Exec("DROP TRIGGER reject_pause")
	s.Stop(ctx, stopRequest(a.AgentID, "stop"), target)
	s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "", nil)
	before, _ = s.Snapshot(ctx, a.AgentID)
	s.db.Exec(`CREATE TRIGGER reject_resume BEFORE INSERT ON events WHEN NEW.kind='agent.resumed' BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if _, err := s.Resume(ctx, resumeRequest(a.AgentID, "failed-resume"), target, 8); err == nil {
		t.Fatal("partial resume accepted")
	}
	after, _ = s.Snapshot(ctx, a.AgentID)
	if after.Agent != before.Agent || len(after.Tasks) != len(before.Tasks) {
		t.Fatal(after, before)
	}
}

func TestSchemaOneMigrationPreservesHistoryAndAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO agents(id,cwd,state,seq) VALUES('legacy','/tmp','idle',1);
INSERT INTO tasks(id,agent_id,state,input) VALUES('task','legacy','accepted','original');
INSERT INTO events(agent_id,seq,kind,payload) VALUES('legacy',1,'fixture','{"unchanged":true}');
INSERT INTO operations(id,fingerprint,result) VALUES('op','hash','{"operation_id":"op","agent_id":"legacy","task_id":"task","state":"accepted"}');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	snap, err := s.Snapshot(ctx, "legacy")
	if err != nil || snap.Agent.Paused || snap.Cursor != 1 || snap.Tasks[0].Input != "original" {
		t.Fatal(snap, err)
	}
	if op, err := s.Operation(ctx, "op"); err != nil || op.TaskID != "task" {
		t.Fatal(op, err)
	}
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
	if _, err := s.db.Exec("DELETE FROM events"); err == nil {
		t.Fatal("migration lost history guard")
	}
}
