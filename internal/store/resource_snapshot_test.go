package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func TestResourceSnapshotSharesRequestBoundaryAndRollsBackNotices(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, a := create(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	p := savedResource(t, "/skills/review/SKILL.md", "original body")
	publication(t, s, a.AgentID, "initial", p, 0)
	before, err := s.ContextSnapshot(ctx, a.AgentID)
	if err != nil || len(before.Resources.Items) != 1 || before.Resources.Items[0].ResourceInfo != p.Info || !before.Resources.Catalog.Valid() || before.Resources.Catalog.Generation > before.Cursor {
		t.Fatal(before, err)
	}
	notices := []ContextMessage{
		{Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "selection"}}}},
		{Resources: before.Resources.Catalog.Entries, Message: model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "resource changes"}}}},
	}
	latest := savedResource(t, p.Info.Source, "new body")
	publication(t, s, a.AgentID, "update", latest, before.Resources.Catalog.Generation)
	// Publication also advances Agent seq. Stale context/catalog cannot commit.
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, 0, before.Cursor, notices, func(int64) any { return map[string]bool{"stale": true} }); err == nil {
		t.Fatal("request mixed old catalog with new context")
	}
	current, err := s.ContextSnapshot(ctx, a.AgentID)
	if err != nil || current.Resources.Catalog.Generation <= before.Resources.Catalog.Generation || current.Resources.Catalog.Entries[0].Version != latest.Info.Version || before.Resources.Catalog.Entries[0].Version != p.Info.Version {
		t.Fatal(before, current, err)
	}
	notices[1].Resources = current.Resources.Catalog.Entries
	// The second notice and request are not independently commit-able.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_request BEFORE INSERT ON events WHEN NEW.kind='model.started' BEGIN SELECT RAISE(ABORT,'injected'); END;`); err != nil {
		t.Fatal(err)
	}
	record := func(cursor int64) any {
		return map[string]any{"context_cursor": cursor, "resource_catalog": current.Resources.Catalog}
	}
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, 0, current.Cursor, notices, record); err == nil {
		t.Fatal("expected rollback")
	}
	after, _ := s.ContextSnapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(current, after) {
		t.Fatal("request rollback leaked a notice")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_request"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, 0, current.Cursor, notices, record); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, After: current.Cursor})
	if err != nil || len(events.Events) != 3 || events.Events[2].Kind != "model.started" {
		t.Fatal(events, err)
	}
	var started struct {
		Cursor  int64                    `json:"context_cursor"`
		Catalog protocol.ResourceCatalog `json:"resource_catalog"`
	}
	if err := json.Unmarshal(events.Events[2].Payload, &started); err != nil || started.Cursor != events.Events[1].Seq || !reflect.DeepEqual(started.Catalog, current.Resources.Catalog) {
		t.Fatal(started, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restored, err := s.ContextSnapshot(ctx, a.AgentID)
	if err != nil || !reflect.DeepEqual(restored.Resources, current.Resources) || len(restored.Messages) != len(current.Messages)+2 {
		t.Fatal(restored, err)
	}
	old, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: a.AgentID, ID: p.Info.ID, Version: p.Info.Version})
	if err != nil || old.Version != p.Info.Version {
		t.Fatal("prior definition lost", old, err)
	}
}
