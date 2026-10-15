package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"zende.local/harness/internal/protocol"
)

func TestActivityScopesTreeAndKeepsHistoryReadOnly(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	makeAgent := func(id string) protocol.Accepted {
		p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "private input must not enter summary"}
		r, _ := protocol.NewRequest("agent.create", id, p)
		a, err := s.Create(ctx, r, p, 8)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	root, child, grand, other := makeAgent("root"), makeAgent("child"), makeAgent("grand"), makeAgent("other")
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE agents SET parent_id=? WHERE id=?", root.AgentID, child.AgentID)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE agents SET parent_id=? WHERE id=?", child.AgentID, grand.AgentID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx, root.AgentID)
	for range 2 {
		a, owners, err := s.Activity(ctx, root.AgentID)
		if err != nil || len(a.Items) != 5 || len(owners) != 3 || owners[other.AgentID] || a.Truncated {
			t.Fatal(a, owners, err)
		}
		for _, item := range a.Items {
			if item.AgentID == other.AgentID || item.ID == root.AgentID {
				t.Fatal("wrong scope", item)
			}
		}
	}
	after, _ := s.Snapshot(ctx, root.AgentID)
	if before.Cursor != after.Cursor || len(after.Tasks) != 1 {
		t.Fatal("read mutated history")
	}
	if _, _, err := s.Activity(ctx, "missing"); err == nil {
		t.Fatal("missing Agent accepted")
	}
	if err := s.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE agents SET parent_id=? WHERE id=?", grand.AgentID, root.AgentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, owners, err := s.Activity(ctx, root.AgentID); err != nil || len(owners) != 3 {
		t.Fatal("cyclic ancestry did not terminate", err)
	}
}

func TestActivityIsBoundedAndExcludesFinishedTasks(t *testing.T) {
	s, _ := newStore(t)
	_, a := create(t, s)
	ctx := context.Background()
	if err := s.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET state='completed' WHERE id=?", a.TaskID)
		if err != nil {
			return err
		}
		for i := 0; i < protocol.MaxActivityItems+5; i++ {
			_, err = tx.Exec("INSERT INTO tasks(id,agent_id,state,input,result) VALUES(?,?,'accepted','','')", fmt.Sprintf("t-%04d", i), a.AgentID)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, _, err := s.Activity(ctx, a.AgentID)
	if err != nil || len(result.Items) != protocol.MaxActivityItems || !result.Truncated {
		t.Fatal(len(result.Items), result.Truncated, err)
	}
	for _, item := range result.Items {
		if item.ID == a.TaskID {
			t.Fatal("finished task included")
		}
	}
}
