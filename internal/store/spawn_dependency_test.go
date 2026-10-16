package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"zende.local/harness/internal/protocol"
)

func spawnDependencyFixture(t *testing.T) (*pluginJournalFixture, Selection, *SpawnDependency, protocol.Request, protocol.SpawnAgent) {
	t.Helper()
	f := newPluginJournal(t, "")
	f.manifest.Capabilities = append(f.manifest.Capabilities, "providers")
	f.publish(t, "2")
	settings, err := f.s.Selection(t.Context(), f.agent)
	if err != nil {
		t.Fatal(err)
	}
	settings.Choice.Model = "plugin:" + f.pkg.Info.ID + ":fixture-models/fixture-text"
	settings.Binding, err = json.Marshal(map[string]any{
		"kind": "plugin-provider", "version": 1, "selection": settings.Choice.Model,
		"resource":       protocol.ResourceRef{ID: f.pkg.Info.ID, Version: f.pkg.Info.Version},
		"definition_ref": f.pkg.Plugin.Providers[0].Ref, "model": "fixture-text",
	})
	if err != nil {
		t.Fatal(err)
	}
	selectReq, _ := protocol.NewRequest("agent.select", "select-plugin", protocol.SelectAgent{AgentID: f.agent, Model: &settings.Choice.Model})
	if _, err := f.s.Select(t.Context(), selectReq, settings, settings.Choice.Revision, true); err != nil {
		t.Fatal(err)
	}
	settings, err = f.s.Selection(t.Context(), f.agent)
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.SpawnAgent{ParentID: f.agent, Input: "child", Context: "none"}
	req, _ := protocol.NewRequest("agent.spawn", "spawn-plugin", p)
	return f, settings, &SpawnDependency{Binding: bytes.Clone(settings.Binding), Package: f.pkg}, req, p
}

func TestSpawnWithDependencyAtomicOwnershipAndDedup(t *testing.T) {
	f, settings, dep, req, p := spawnDependencyFixture(t)
	s := f.s
	accepted, publication, err := s.SpawnWithDependency(t.Context(), req, p, settings.Choice.Cwd, settings.Choice.Revision, 8, 4, dep)
	if err != nil || publication == nil || publication.Resource != dep.Package.Info {
		t.Fatal(accepted, publication, err)
	}
	child := protocol.ResourceTarget{AgentID: accepted.AgentID, ID: dep.Package.Info.ID}
	pkg, generation, err := s.PublishedResource(t.Context(), child)
	if err != nil || generation != publication.Generation || !reflect.DeepEqual(pkg, dep.Package) {
		t.Fatal(pkg, generation, err)
	}
	selection, err := s.Selection(t.Context(), accepted.AgentID)
	if err != nil || !bytes.Equal(selection.Binding, settings.Binding) {
		t.Fatal(selection, err)
	}
	// The receipt wins over changed/missing preparation and stale revision.
	again, nextPublication, err := s.SpawnWithDependency(t.Context(), req, p, "/irrelevant", -1, 0, 0, nil)
	if err != nil || !reflect.DeepEqual(accepted, again) || nextPublication != nil {
		t.Fatal(again, nextPublication, err)
	}
	events, err := s.Events(t.Context(), protocol.Cursor{AgentID: accepted.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	var published, taskAccepted int64
	for _, event := range events.Events {
		if event.Kind == "resource.published" {
			published = event.Seq
		}
		if event.Kind == "task.accepted" {
			taskAccepted = event.Seq
		}
	}
	if published != publication.Generation || published >= taskAccepted {
		t.Fatal(published, taskAccepted)
	}
	other, _ := protocol.NewRequest("agent.create", "unrelated", protocol.CreateAgent{Cwd: settings.Choice.Cwd})
	a, err := s.Create(t.Context(), other, protocol.CreateAgent{Cwd: settings.Choice.Cwd}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: a.AgentID, ID: child.ID, Version: pkg.Info.Version}); err == nil {
		t.Fatal("unrelated Agent inherited dependency")
	}
}

func TestSpawnWithDependencyRollsBackEveryProjection(t *testing.T) {
	for _, table := range []string{"agents", "agent_selections", "resource_publications", "agent_resources", "tasks", "operations"} {
		t.Run(table, func(t *testing.T) {
			f, settings, dep, req, p := spawnDependencyFixture(t)
			s := f.s
			tables := []string{"agents", "agent_selections", "resource_versions", "resource_publications", "agent_resources", "tasks", "operations", "events"}
			before := map[string]int{}
			for _, name := range tables {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + name).Scan(&count); err != nil {
					t.Fatal(err)
				}
				before[name] = count
			}
			if _, err := s.db.Exec("CREATE TRIGGER fail_spawn BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'injected spawn failure'); END"); err != nil {
				t.Fatal(err)
			}
			accepted, publication, err := s.SpawnWithDependency(t.Context(), req, p, settings.Choice.Cwd, settings.Choice.Revision, 8, 4, dep)
			if err == nil || accepted.AgentID != "" || publication != nil {
				t.Fatal("partial acceptance", accepted, publication, err)
			}
			for _, name := range tables {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + name).Scan(&count); err != nil || count != before[name] {
					t.Fatal("escaped rollback", name, count, before[name], err)
				}
			}
			if _, found, err := s.AcceptedRequest(t.Context(), req); err != nil || found {
				t.Fatal(found, err)
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_spawn"); err != nil {
				t.Fatal(err)
			}
			if _, publication, err := s.SpawnWithDependency(t.Context(), req, p, settings.Choice.Cwd, settings.Choice.Revision, 8, 4, dep); err != nil || publication == nil {
				t.Fatal(publication, err)
			}
		})
	}
}

func TestSpawnWithDependencyRejectsWrongSnapshotOrOwnership(t *testing.T) {
	for _, failure := range []string{"missing", "binding", "package", "revision", "foreign-parent"} {
		t.Run(failure, func(t *testing.T) {
			f, settings, dep, req, p := spawnDependencyFixture(t)
			revision := settings.Choice.Revision
			switch failure {
			case "missing":
				dep = nil
			case "binding":
				dep.Binding = json.RawMessage(`{}`)
			case "package":
				dep.Package.Info.Version = "wrong"
			case "revision":
				revision--
			case "foreign-parent":
				create, _ := protocol.NewRequest("agent.create", "other", protocol.CreateAgent{Cwd: settings.Choice.Cwd})
				other, err := f.s.Create(t.Context(), create, protocol.CreateAgent{Cwd: settings.Choice.Cwd}, 8)
				if err != nil {
					t.Fatal(err)
				}
				settings.Choice.AgentID = other.AgentID
				selectReq, _ := protocol.NewRequest("agent.select", "select-foreign", protocol.SelectAgent{AgentID: other.AgentID, Model: &settings.Choice.Model})
				selected, err := f.s.Select(t.Context(), selectReq, settings, 0, true)
				if err != nil {
					t.Fatal(err)
				}
				p.ParentID, revision = other.AgentID, selected.Selection.Revision
				req, _ = protocol.NewRequest("agent.spawn", "spawn-foreign", p)
			}
			before, _ := f.s.ListAgents(t.Context())
			accepted, publication, err := f.s.SpawnWithDependency(t.Context(), req, p, settings.Choice.Cwd, revision, 8, 4, dep)
			if err == nil || accepted.AgentID != "" || publication != nil {
				t.Fatal(accepted, publication, err)
			}
			after, _ := f.s.ListAgents(t.Context())
			if !reflect.DeepEqual(before, after) {
				t.Fatal("partial child", after)
			}
			if _, found, err := f.s.AcceptedRequest(t.Context(), req); err != nil || found {
				t.Fatal(found, err)
			}
		})
	}
}
