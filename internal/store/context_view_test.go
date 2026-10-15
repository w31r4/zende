package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func contextText(role, value string) model.Message {
	return model.Message{Role: role, Content: []model.Block{{Type: "text", Text: value}}}
}

func rawEntries(t *testing.T, s *Store, agent string) []contextEntry {
	t.Helper()
	var entries []contextEntry
	if err := s.Transaction(context.Background(), func(tx *sql.Tx) error {
		var err error
		entries, err = rawContextEntries(context.Background(), tx, agent, 0, 1<<62)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func spawnWithContext(t *testing.T, s *Store, p protocol.SpawnAgent) protocol.Accepted {
	t.Helper()
	selection, err := s.Selection(context.Background(), p.ParentID)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), p)
	a, err := s.Spawn(context.Background(), req, p, selection.Choice.Cwd, selection.Choice.Revision, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestContextViewAppendOnlyTailInheritanceAndRestart(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, parent := create(t, s)
	_, _ = s.ClaimNext(ctx, parent.AgentID)
	addSpawnContext(t, s, parent.AgentID, parent.TaskID,
		contextText("assistant", "old answer"), contextText("user", "recent question"), contextText("assistant", "recent answer"))
	before, err := s.ContextSnapshot(ctx, parent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawEntries(t, s, parent.AgentID)
	prepared := ContextView{SourceCursor: before.Cursor, Through: raw[1].Seq, Summary: "Earlier goal and result; old raw records remain available."}
	// New messages arrive while the summary is being prepared.
	addSpawnContext(t, s, parent.AgentID, parent.TaskID, contextText("user", "concurrent tail"))
	view, err := s.PublishContextView(ctx, parent.AgentID, prepared)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.ContextSnapshot(ctx, parent.AgentID)
	if err != nil || snapshot.View.ID != view.ID || len(snapshot.Messages) != 4 || snapshot.Messages[3].Content[0].Text != "concurrent tail" || !strings.Contains(snapshot.Messages[0].Content[0].Text, view.Summary) {
		t.Fatal(snapshot, err)
	}
	if got := rawEntries(t, s, parent.AgentID); len(got) != len(raw)+1 || !reflect.DeepEqual(raw, got[:len(raw)]) {
		t.Fatal("raw context changed")
	}
	// Before publication the same cursor still resolves the old view.
	if err := s.Transaction(ctx, func(tx *sql.Tx) error {
		old, err := contextEntries(ctx, tx, parent.AgentID, before.Cursor)
		if err == nil && !reflect.DeepEqual(old, raw) {
			t.Error("historical view changed", old)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	child := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "child"})
	if child.Spawn.ViewID != view.ID || !child.Spawn.SummaryIncluded || child.Spawn.Messages != 4 || child.Spawn.Turns != 2 || child.Spawn.First != raw[2].Seq {
		t.Fatal(child.Spawn)
	}
	childMessages, _, err := s.ModelContext(ctx, child.AgentID)
	if err != nil || !reflect.DeepEqual(childMessages[:4], snapshot.Messages) {
		t.Fatal(childMessages, err)
	}
	// Copied summaries remain summaries, not additional user turns.
	grandchild := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: child.AgentID, Input: "grandchild", Context: "last:1"})
	if grandchild.Spawn.Turns != 1 || grandchild.Spawn.Messages != 2 {
		t.Fatal(grandchild.Spawn)
	}
	// A replacement view includes the previous summary and preserves the tail.
	next, err := s.PublishContextView(ctx, parent.AgentID, ContextView{Previous: view.ID, SourceCursor: snapshot.Cursor, Through: raw[3].Seq, Summary: "Updated summary of both earlier rounds."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishContextView(ctx, parent.AgentID, prepared); err == nil {
		t.Fatal("stale preparation published")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ContextSnapshot(ctx, parent.AgentID)
	if err != nil || after.View.ID != next.ID || len(after.Messages) != 2 || after.Messages[1].Content[0].Text != "concurrent tail" {
		t.Fatal(after, err)
	}
	if got := rawEntries(t, reopened, parent.AgentID); !reflect.DeepEqual(raw, got[:len(raw)]) {
		t.Fatal("raw records changed after restart")
	}
}

func TestContextViewRejectsSplitInvalidRangeAndRollsBack(t *testing.T) {
	for _, variant := range []string{"split", "event-not-message", "future", "empty", "invalid-utf8", "rollback"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, parent := create(t, s)
			_, _ = s.ClaimNext(ctx, parent.AgentID)
			addSpawnContext(t, s, parent.AgentID, parent.TaskID,
				model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "one"}, {Type: "tool_call", CallID: "two"}}},
				model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "one"}}},
				model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "two"}}})
			snap, _ := s.ContextSnapshot(ctx, parent.AgentID)
			raw := rawEntries(t, s, parent.AgentID)
			p := ContextView{SourceCursor: snap.Cursor, Through: raw[3].Seq, Summary: "summary"}
			switch variant {
			case "split":
				p.Through = raw[2].Seq
			case "event-not-message":
				p.Through = 1
			case "future":
				p.SourceCursor++
			case "empty":
				p.Summary = " "
			case "invalid-utf8":
				p.Summary = string([]byte{255})
			case "rollback":
				if _, err := s.db.Exec("CREATE TRIGGER fail_view BEFORE INSERT ON events WHEN NEW.kind='context.view' BEGIN SELECT RAISE(ABORT,'test rollback'); END"); err != nil {
					t.Fatal(err)
				}
			}
			if v, err := s.PublishContextView(ctx, parent.AgentID, p); err == nil || v.ID != "" {
				t.Fatal(v, err)
			}
			after, _ := s.ContextSnapshot(ctx, parent.AgentID)
			if !reflect.DeepEqual(snap, after) {
				t.Fatal("failed publication changed view")
			}
		})
	}
}

func TestContextViewConcurrentPublicationFencesRequests(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, parent := create(t, s)
	_, _ = s.ClaimNext(ctx, parent.AgentID)
	snap, _ := s.ContextSnapshot(ctx, parent.AgentID)
	p := ContextView{SourceCursor: snap.Cursor, Through: snap.Cursor, Summary: "summary"}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, err := s.PublishContextView(ctx, parent.AgentID, p); results <- err })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("competing publications accepted", accepted)
	}
	onlySummary := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "summary only", Context: "last:1"})
	if onlySummary.Spawn.Messages != 1 || onlySummary.Spawn.Turns != 0 || !onlySummary.Spawn.SummaryIncluded || onlySummary.Spawn.First != 0 {
		t.Fatal(onlySummary.Spawn)
	}
	if err := s.BeginModelRequest(ctx, parent.AgentID, parent.TaskID, 0, snap.Cursor, "", func(int64) any { return json.RawMessage(`{}`) }); err == nil {
		t.Fatal("stale request crossed view change")
	}
}

func TestContextViewJournalMigrationPreservesVersionSevenHistory(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, parent := create(t, s)
	_, _ = s.ClaimNext(ctx, parent.AgentID)
	before, _ := s.ContextSnapshot(ctx, parent.AgentID)
	if _, err := s.db.Exec("DROP TABLE workspace_operations; PRAGMA user_version=7"); err != nil {
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
	after, err := reopened.ContextSnapshot(ctx, parent.AgentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	if _, err := reopened.PublishContextView(ctx, parent.AgentID, ContextView{SourceCursor: after.Cursor, Through: after.Cursor, Summary: "migrated history"}); err != nil {
		t.Fatal(err)
	}
}
