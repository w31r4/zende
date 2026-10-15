package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"zende.local/harness/internal/protocol"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestSnapshotRacesTaskCommits(t *testing.T) {
	s, _ := newStore(t)
	_, a := create(t, s)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 40 {
			p := protocol.Submit{AgentID: a.AgentID, Input: "queued"}
			r, _ := protocol.NewRequest("agent.submit", fmt.Sprint("race-", i), p)
			if _, err := s.Submit(ctx, r, p, 1); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 40 {
		snap, err := s.Snapshot(ctx, a.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(snap.Tasks)) != snap.Cursor-1 {
			t.Fatalf("inconsistent snapshot: tasks=%d cursor=%d", len(snap.Tasks), snap.Cursor)
		}
	}
	<-done
	page, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil || len(page.Events) != 42 {
		t.Fatal(len(page.Events), err)
	}
}

func TestCapacityCountsActiveSessionsNotHistory(t *testing.T) {
	s, _ := newStore(t)
	_, a := create(t, s)
	ctx := context.Background()
	empty := protocol.CreateAgent{Cwd: t.TempDir()}
	r, _ := protocol.NewRequest("agent.create", "empty", empty)
	b, err := s.Create(ctx, r, empty, 1)
	if err != nil {
		t.Fatal("idle session should not consume active capacity", err)
	}
	p := protocol.Submit{AgentID: b.AgentID, Input: "would exceed capacity"}
	r, _ = protocol.NewRequest("agent.submit", "excess", p)
	if _, err := s.Submit(ctx, r, p, 1); err == nil {
		t.Fatal("exceeded active capacity")
	}
	p.AgentID = a.AgentID
	r, _ = protocol.NewRequest("agent.submit", "same-active", p)
	if _, err := s.Submit(ctx, r, p, 1); err != nil {
		t.Fatal("queue created another active agent", err)
	}
}
func create(t *testing.T, s *Store) (protocol.Request, protocol.Accepted) {
	t.Helper()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "work"}
	r, _ := protocol.NewRequest("agent.create", "stable", p)
	a, err := s.Create(context.Background(), r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return r, a
}

func TestDurableAcceptanceAndDedup(t *testing.T) {
	s, path := newStore(t)
	r, a := create(t, s)
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var p protocol.CreateAgent
	if err := protocol.StrictDecode(r.Params, &p); err != nil {
		t.Fatal(err)
	}
	r.ID = protocol.NewID("rpc")
	again, err := s.Create(context.Background(), r, p, 1)
	if err != nil || again != a {
		t.Fatal(again, err)
	}
	snap, err := s.Snapshot(context.Background(), a.AgentID)
	if err != nil || len(snap.Tasks) != 1 || snap.Cursor != 2 {
		t.Fatal(snap, err)
	}
	r.Params = []byte(`{"cwd":"/different"}`)
	if _, err := s.Create(context.Background(), r, p, 8); err == nil {
		t.Fatal("request identity reused")
	}
}

func TestAtomicRollbackAndHistoryGuards(t *testing.T) {
	s, _ := newStore(t)
	_, a := create(t, s)
	ctx := context.Background()
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := appendEvent(ctx, tx, a.AgentID, a.TaskID, "uncommitted", map[string]bool{"ok": true}); err != nil {
			return err
		}
		return errors.New("injected failure")
	})
	if err == nil {
		t.Fatal("injection lost")
	}
	snap, _ := s.Snapshot(ctx, a.AgentID)
	if snap.Cursor != 2 {
		t.Fatal("transaction leaked event")
	}
	for _, q := range []string{"UPDATE events SET kind='overwrite'", "DELETE FROM events", "UPDATE operations SET result='{}'", "DELETE FROM operations"} {
		if _, err := s.db.Exec(q); err == nil {
			t.Fatal("immutable record changed", q)
		}
	}
	if _, err := s.db.Exec("CREATE TRIGGER fail_accept BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'injected commit failure'); END;"); err != nil {
		t.Fatal(err)
	}
	p := protocol.Submit{AgentID: a.AgentID, Input: "not accepted"}
	r, _ := protocol.NewRequest("agent.submit", "failing", p)
	accepted, err := s.Submit(ctx, r, p, 8)
	if err == nil || accepted.AgentID != "" {
		t.Fatal("returned acceptance before commit", accepted, err)
	}
	snap, _ = s.Snapshot(ctx, a.AgentID)
	if len(snap.Tasks) != 1 || snap.Cursor != 2 {
		t.Fatal("failed acceptance leaked task")
	}
}

func TestConcurrentRetryAndCursorBoundary(t *testing.T) {
	s, _ := newStore(t)
	_, a := create(t, s)
	ctx := context.Background()
	p := protocol.Submit{AgentID: a.AgentID, Input: "once"}
	r, _ := protocol.NewRequest("agent.submit", "concurrent", p)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Submit(ctx, r, p, 8); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	snap, err := s.Snapshot(ctx, a.AgentID)
	if err != nil || len(snap.Tasks) != 2 {
		t.Fatal(snap, err)
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Append(ctx, a.AgentID, a.TaskID, "progress", struct{}{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	page, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, After: snap.Cursor, Limit: 7})
	if err != nil || len(page.Events) != 7 || page.Events[0].Seq != snap.Cursor+1 {
		t.Fatal(page, err)
	}
	count := len(page.Events)
	for page.Next < page.Latest {
		page, err = s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, After: page.Next, Limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Events)
	}
	if count != 20 {
		t.Fatal("missing events", count)
	}
	if _, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, After: 10000}); err == nil {
		t.Fatal("future cursor accepted")
	}
}

func TestSchemaRejectionAndCancelledCommit(t *testing.T) {
	s, path := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := protocol.CreateAgent{Cwd: "/tmp"}
	r, _ := protocol.NewRequest("agent.create", "cancelled", p)
	if accepted, err := s.Create(ctx, r, p, 8); err == nil || accepted.AgentID != "" {
		t.Fatal(accepted, err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if unexpected, err := Open(path); err == nil {
		unexpected.Close()
		t.Fatal("future schema accepted")
	}
}
