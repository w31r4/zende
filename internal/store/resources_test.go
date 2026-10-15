package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func savedResource(t *testing.T, source, body string) resource.Package {
	t.Helper()
	p, err := resource.BuildPackage("skill", source, "SKILL.md", []resource.File{
		{Path: "SKILL.md", Data: []byte("---\nname: review\ndescription: 中文 review\n---\n" + body)},
		{Path: "assets/a.bin", Data: []byte{0, 255, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func publication(t *testing.T, s *Store, agent, operation string, p resource.Package, base int64) (protocol.Request, protocol.Accepted) {
	t.Helper()
	r, err := protocol.NewRequest("resource.register", operation, protocol.RegisterResource{AgentID: agent, Kind: p.Info.Kind, Path: p.Info.Source})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.PublishResource(context.Background(), r, agent, p, base)
	if err != nil || a.Resource == nil || *a.Resource != p.Info {
		t.Fatal(a, err)
	}
	return r, a
}

func TestResourceVersionsSurviveRestartAndStayAgentScoped(t *testing.T) {
	ctx := context.Background()
	s, db := newStore(t)
	_, a := create(t, s)
	p := savedResource(t, "/installed/review/SKILL.md", "original instructions")
	r, accepted := publication(t, s, a.AgentID, "publish", p, 0)
	status, err := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: p.Info.ID})
	if err != nil || status.State != "ready" || status.Generation == 0 {
		t.Fatal(status, err)
	}
	// Idempotency precedes package validation or future generation checks.
	again, err := s.PublishResource(ctx, r, a.AgentID, resource.Package{}, -1)
	if err != nil || !reflect.DeepEqual(accepted, again) {
		t.Fatal("retry changed receipt", again, err)
	}
	publication(t, s, a.AgentID, "unchanged", p, status.Generation)
	changed := savedResource(t, p.Info.Source, "updated instructions")
	publication(t, s, a.AgentID, "refresh", changed, status.Generation)
	page, err := s.Resources(ctx, protocol.ResourceList{AgentID: a.AgentID})
	if err != nil || len(page.Items) != 1 || page.Items[0] != changed.Info || page.Generation <= status.Generation {
		t.Fatal(page, err)
	}
	events, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events.Events {
		if e.Kind == "resource.published" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("duplicate publication", count)
	}
	s.Close()
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, expected := range []resource.Package{p, changed} {
		content, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: a.AgentID, ID: p.Info.ID, Version: expected.Info.Version})
		if err != nil || !strings.Contains(content.Text, expected.Instructions) || content.Source != expected.Info.Source || content.Version != expected.Info.Version {
			t.Fatal(content, err)
		}
		asset, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: a.AgentID, ID: p.Info.ID, Version: expected.Info.Version, Path: "assets/a.bin"})
		if err != nil || !reflect.DeepEqual(asset.Data, []byte{0, 255, 1}) {
			t.Fatal(asset, err)
		}
	}
	other := protocol.CreateAgent{Cwd: t.TempDir()}
	or, _ := protocol.NewRequest("agent.create", "other-agent", other)
	b, err := s.Create(ctx, or, other, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: b.AgentID, ID: p.Info.ID, Version: p.Info.Version}); err == nil {
		t.Fatal("read another Agent publication")
	}
	publication(t, s, b.AgentID, "other-publish", changed, 0)
	if _, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: b.AgentID, ID: p.Info.ID, Version: p.Info.Version}); err == nil {
		t.Fatal("current registration exposed unpublished history")
	}
	if _, err := s.ReadResource(ctx, protocol.ResourceRead{AgentID: b.AgentID, ID: p.Info.ID}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"UPDATE resource_versions SET package='{}'", "DELETE FROM resource_versions", "UPDATE resource_publications SET version='changed'", "DELETE FROM resource_publications"} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatal("mutated retained resource", statement)
		}
	}
}

func TestResourcePublishRollsBackEveryProjection(t *testing.T) {
	for _, table := range []string{"resource_versions", "events", "resource_publications", "agent_resources", "operations"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			_, a := create(t, s)
			p := savedResource(t, "/installed/review/SKILL.md", "body")
			before, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if _, err := s.db.Exec("CREATE TRIGGER fail_resource BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'injected resource failure'); END"); err != nil {
				t.Fatal(err)
			}
			r, _ := protocol.NewRequest("resource.register", "publish", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: p.Info.Source})
			if _, err := s.PublishResource(ctx, r, a.AgentID, p, 0); err == nil {
				t.Fatal("expected failure")
			}
			after, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
			if !reflect.DeepEqual(before, after) {
				t.Fatal("event escaped rollback")
			}
			for _, name := range []string{"resource_versions", "resource_publications", "agent_resources"} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + name).Scan(&count); err != nil || count != 0 {
					t.Fatal(name, count, err)
				}
			}
			if _, found, err := s.AcceptedRequest(ctx, r); err != nil || found {
				t.Fatal("accepted partial publication", found, err)
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_resource"); err != nil {
				t.Fatal(err)
			}
			publication(t, s, a.AgentID, "publish", p, 0)
		})
	}
}

func TestResourceConcurrentPreparationCannotRevertPublication(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	p := savedResource(t, "/installed/review/SKILL.md", "v1")
	publication(t, s, a.AgentID, "initial", p, 0)
	status, _ := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: a.AgentID, ID: p.Info.ID})
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			candidate := savedResource(t, p.Info.Source, fmt.Sprint("v", i+2))
			r, _ := protocol.NewRequest("resource.refresh", fmt.Sprint("refresh", i), protocol.ResourceTarget{AgentID: a.AgentID, ID: p.Info.ID})
			_, err := s.PublishResource(ctx, r, a.AgentID, candidate, status.Generation)
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	success, rejected := 0, 0
	for err := range errors {
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "published during preparation") {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatal(success, rejected)
	}
	// Even an old snapshot that used to be published cannot roll back the pointer.
	r, _ := protocol.NewRequest("resource.refresh", "stale", protocol.ResourceTarget{AgentID: a.AgentID, ID: p.Info.ID})
	if _, err := s.PublishResource(ctx, r, a.AgentID, p, status.Generation); err == nil {
		t.Fatal("stale preparation reverted pointer")
	}
}

func TestResourceMetadataPaginationAndValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	for i := range 5 {
		publication(t, s, a.AgentID, fmt.Sprint("publish", i), savedResource(t, fmt.Sprintf("/skills/%d/SKILL.md", i), "body-not-in-list"), 0)
	}
	query := protocol.ResourceList{AgentID: a.AgentID, Kind: "skill", Query: "REVIEW", Limit: 2}
	seen := map[string]bool{}
	for {
		page, err := s.Resources(ctx, query)
		if err != nil || len(page.Items) == 0 || page.Generation == 0 {
			t.Fatal(page, err)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate page item")
			}
			seen[item.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.After = page.Next
	}
	if len(seen) != 5 {
		t.Fatal(seen)
	}
	if page, err := s.Resources(ctx, protocol.ResourceList{AgentID: a.AgentID, Query: "中文"}); err != nil || len(page.Items) != 5 {
		t.Fatal(page, err)
	}
	for _, q := range []protocol.ResourceList{{AgentID: a.AgentID, Kind: "unknown"}, {AgentID: a.AgentID, Limit: 101}, {AgentID: a.AgentID, Limit: -1}, {AgentID: a.AgentID, Query: strings.Repeat("x", 257)}, {AgentID: "absent"}} {
		if _, err := s.Resources(ctx, q); err == nil {
			t.Fatal("accepted invalid query", q)
		}
	}
	p := savedResource(t, "/skills/invalid/SKILL.md", "body")
	p.Info.Description = "changed without new version"
	r, _ := protocol.NewRequest("resource.register", "invalid", protocol.RegisterResource{AgentID: a.AgentID, Kind: "skill", Path: p.Info.Source})
	if _, err := s.PublishResource(ctx, r, a.AgentID, p, 0); err == nil {
		t.Fatal("accepted tampered metadata")
	}
}

func TestSchemaFourMigrationKeepsQuestionsAndImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(schema + "ALTER TABLE agents ADD COLUMN paused INTEGER NOT NULL DEFAULT 0; ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;" + attachmentSchema + questionSchema)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO agents(id,cwd,state) VALUES('a','/tmp','idle'); INSERT INTO tasks(id,agent_id,state,input,attachments) VALUES('t','a','accepted','original','[\"image\"]'); INSERT INTO questions(id,agent_id,task_id,prompt,state,answer,attachments) VALUES('q','a','t','kept question','answered','kept answer','[\"image\"]');")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO attachments(id,agent_id,name,media_type,sha256,bytes,width,height,data) VALUES('image','a','original.png','image/png','digest',3,1,1,?)", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
	p := savedResource(t, "/skills/migrated/SKILL.md", "body")
	publication(t, s, "a", "migrated-resource", p, 0)
	snap, err := s.Snapshot(context.Background(), "a")
	if err != nil || len(snap.Tasks) != 1 || snap.Tasks[0].Input != "original" || !reflect.DeepEqual(snap.Tasks[0].Attachments, []string{"image"}) {
		t.Fatal(snap, err)
	}
	q, err := s.Question(context.Background(), protocol.QuestionTarget{AgentID: "a", ID: "q"})
	if err != nil || q.Answer != "kept answer" || !reflect.DeepEqual(q.Attachments, []string{"image"}) {
		t.Fatal(q, err)
	}
	var data []byte
	if err := s.db.QueryRow("SELECT data FROM attachments WHERE id='image'").Scan(&data); err != nil || !reflect.DeepEqual(data, []byte{1, 2, 3}) {
		t.Fatal(data, err)
	}
}
