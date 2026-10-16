package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func spawnPluginRequest(t *testing.T, c *Core, parent, mode string) (protocol.Request, protocol.SpawnAgent) {
	t.Helper()
	p := protocol.SpawnAgent{ParentID: parent, Input: "child task", Context: "none"}
	if mode == "worktree" {
		settings, err := c.Store.Selection(t.Context(), parent)
		if err != nil {
			t.Fatal(err)
		}
		dir := settings.Choice.Cwd
		worktreeTestGit(t, dir, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("parent"), 0600); err != nil {
			t.Fatal(err)
		}
		worktreeTestGit(t, dir, "add", "tracked")
		worktreeTestGit(t, dir, "commit", "-m", "base")
		p.Workspace, p.WorktreePath = "worktree", filepath.Join(t.TempDir(), "child")
	}
	req, err := protocol.NewRequest("agent.spawn", "spawn-plugin", p)
	if err != nil {
		t.Fatal(err)
	}
	return req, p
}

func spawnAudit(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertSpawnUnaccepted(t *testing.T, c *Core, req protocol.Request, parent string, before protocol.EventPage) {
	t.Helper()
	agents, err := c.Store.ListAgents(t.Context())
	if err != nil || len(agents) != 1 || agents[0].ID != parent {
		t.Fatal("partial child", agents, err)
	}
	if _, found, err := c.Store.AcceptedRequest(t.Context(), req); err != nil || found {
		t.Fatal("partial acceptance", found, err)
	}
	after, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: parent})
	// Worktree preparation has its own durable events, but no agent.spawned.
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range after.Events {
		if event.Kind == "agent.spawned" {
			t.Fatal("failed dependency created a child event", event)
		}
	}
	if before.Events != nil && !reflect.DeepEqual(before, after) {
		t.Fatal("failed spawn changed parent context")
	}
	c.pluginMu.Lock()
	defer c.pluginMu.Unlock()
	if len(c.pluginSlots) != 1 || len(c.pluginOwned) != 1 {
		t.Fatal("failed candidate retained by Core", len(c.pluginSlots), len(c.pluginOwned))
	}
}

func TestPluginSpawnReadyOwnedDependencyAndChildCompletion(t *testing.T) {
	for _, mode := range []string{"normal", "worktree"} {
		t.Run(mode, func(t *testing.T) {
			c, parentTarget, manifest, source, selection := registeredModelPlugin(t, "")
			effort := "high"
			choose(t, c, protocol.SelectAgent{AgentID: parentTarget.AgentID, Model: &selection, Effort: &effort})
			settings, err := c.Store.Selection(t.Context(), parentTarget.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			parentInstance := c.pluginSlots[parentTarget].instance
			req, p := spawnPluginRequest(t, c, parentTarget.AgentID, mode)
			accepted, err := c.spawnAgent(t.Context(), req, p)
			if err != nil || accepted.AgentID == "" || accepted.TaskID == "" {
				t.Fatal(accepted, err)
			}
			childTarget := protocol.ResourceTarget{AgentID: accepted.AgentID, ID: parentTarget.ID}
			slot := c.pluginSlots[childTarget]
			if slot == nil || slot.instance == parentInstance || slot.instance.Info().State != "ready" || c.pluginOwned[slot] != childTarget {
				t.Fatal("child did not get an independent ready owned instance", slot)
			}
			childSettings, err := c.Store.Selection(t.Context(), accepted.AgentID)
			if err != nil || !bytes.Equal(settings.Binding, childSettings.Binding) || childSettings.Choice.Model != selection || childSettings.Choice.Effort != effort {
				t.Fatal(childSettings, err)
			}
			pkg, generation, err := c.Store.PublishedResource(t.Context(), childTarget)
			if err != nil || pkg.Info.Source != source || pkg.Info.Version != slot.version || generation != slot.generation {
				t.Fatal(pkg.Info, generation, err)
			}
			if mode == "worktree" && (accepted.Workspace == nil || childSettings.Choice.Cwd != p.WorktreePath) {
				t.Fatal("worktree cwd not inherited", accepted, childSettings)
			}
			if bytes.Contains(spawnAudit(t), []byte("plugin.model.stream")) {
				t.Fatal("preparation sent a model request")
			}
			// A later parent refresh/selection must not alter the child's binding,
			// process or request version. Its executable source need not stay v1.
			manifest.Version = "2"
			saveRuntimePlugin(t, source, manifest)
			refresh, _ := protocol.NewRequest("resource.refresh", "refresh-parent", parentTarget)
			if _, err := c.Handle(t.Context(), refresh); err != nil {
				t.Fatal(err)
			}
			choose(t, c, protocol.SelectAgent{AgentID: parentTarget.AgentID, Model: &selection})
			c.Tools, err = tool.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			runCore(t, c)
			snap := waitTask(t, c, accepted, "completed")
			for _, task := range snap.Tasks {
				if task.ID != accepted.TaskID {
					continue
				}
				var result struct {
					Scope   plugin.Scope `json:"scope"`
					Version string       `json:"version"`
					Effort  string       `json:"effort"`
				}
				if err := json.Unmarshal([]byte(task.Result), &result); err != nil || result.Scope.AgentID != accepted.AgentID || result.Scope.TaskID != accepted.TaskID || result.Scope.Cwd != childSettings.Choice.Cwd || result.Version != "1" || result.Effort != effort {
					t.Fatal("child used wrong scope/version", result, err)
				}
			}
			events, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: accepted.AgentID, Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			var published, taskAccepted, started int64
			for _, event := range events.Events {
				switch event.Kind {
				case "resource.published":
					published = event.Seq
				case "task.accepted":
					taskAccepted = event.Seq
				case "model.started":
					started = event.Seq
					var record requestRecord
					if json.Unmarshal(event.Payload, &record) != nil || record.PluginBinding == nil || record.PluginBinding.InstanceID != slot.instance.Info().InstanceID || record.PluginBinding.Generation != generation || record.PluginBinding.Resource.Version != pkg.Info.Version {
						t.Fatal("wrong child model binding", record)
					}
				}
			}
			if published != generation || published >= taskAccepted || taskAccepted >= started {
				t.Fatal("dependency was not published before runnable task", published, taskAccepted, started)
			}
			// Retry even after the source stops being usable: no startup, new
			// task, or replacement instance is allowed.
			if err := os.WriteFile(source, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			before := spawnAudit(t)
			again, err := c.spawnAgent(t.Context(), req, p)
			if err != nil || !reflect.DeepEqual(accepted, again) || c.pluginSlots[childTarget] != slot || !bytes.Equal(before, spawnAudit(t)) {
				t.Fatal("retry changed accepted spawn or started a process", again, err)
			}
		})
	}
}

func TestPluginSpawnPreparationFailureLeavesNoChild(t *testing.T) {
	for _, mode := range []string{"normal", "worktree"} {
		for _, failure := range []string{"invalid-provider", "changed-version"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				c, target, manifest, source, selection := registeredModelPlugin(t, "")
				choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
				req, p := spawnPluginRequest(t, c, target.AgentID, mode)
				before, _ := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID})
				if mode == "worktree" {
					before = protocol.EventPage{}
				}
				if failure == "changed-version" {
					manifest.Version = "2"
					saveRuntimePlugin(t, source, manifest)
				} else {
					t.Setenv("ZENDE_TEST_PLUGIN_MODE", failure)
				}
				accepted, err := c.spawnAgent(t.Context(), req, p)
				if err == nil || accepted.AgentID != "" || accepted.TaskID != "" {
					t.Fatal("failed preparation exposed a child", accepted, err)
				}
				if failure == "changed-version" && !isFailure(err, "stale_reference") {
					t.Fatal(err)
				}
				assertSpawnUnaccepted(t, c, req, target.AgentID, before)
				if bytes.Contains(spawnAudit(t), []byte("plugin.model.stream")) {
					t.Fatal("failed dependency sent a model request")
				}
				// Failure leaves the same operation retryable, including an
				// already-created, owned worktree.
				manifest.Version = "1"
				saveRuntimePlugin(t, source, manifest)
				t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
				if accepted, err = c.spawnAgent(t.Context(), req, p); err != nil || accepted.AgentID == "" {
					t.Fatal(accepted, err)
				}
			})
		}
	}
}

func TestPluginSpawnParentRevisionFenceAndOfflineReadiness(t *testing.T) {
	c, target, _, _, selection := registeredModelPlugin(t, "")
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection})
	settings, _ := c.Store.Selection(t.Context(), target.AgentID)
	req, p := spawnPluginRequest(t, c, target.AgentID, "normal")
	// Save the old preparation inputs, then edit the parent before publication.
	effort := "high"
	choose(t, c, protocol.SelectAgent{AgentID: target.AgentID, Model: &selection, Effort: &effort})
	before, _ := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID})
	accepted, err := c.spawnWithModelDependency(t.Context(), req, p, settings.Choice.Cwd, settings)
	if !isFailure(err, "selection_changed") || accepted.AgentID != "" {
		t.Fatal(accepted, err)
	}
	assertSpawnUnaccepted(t, c, req, target.AgentID, before)
	accepted, err = c.spawnAgent(t.Context(), req, p)
	if err != nil {
		t.Fatal(err)
	}
	childSettings, _ := c.Store.Selection(t.Context(), accepted.AgentID)
	// A reopened Core can reconstruct exact metadata but cannot dispatch until
	// it has its own ready instance. Retained publication is not a live lease.
	next := New(c.Store, c.Config)
	binding, err := next.restoreBinding(t.Context(), accepted.AgentID, childSettings.Binding)
	if err != nil {
		t.Fatal(err)
	}
	_, _, release, err := next.prepareModelRequest(binding, plugin.Scope{AgentID: accepted.AgentID, TaskID: accepted.TaskID, Cwd: childSettings.Choice.Cwd}, "offline-child")
	if !isFailure(err, "unavailable") || release != nil {
		t.Fatal("snapshot implied readiness", err)
	}
	// Store receipt also survives reopening and never republishes dependencies.
	reopened, err := store.Open(filepath.Join(c.Config.Config.Cwd, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if old, found, err := reopened.AcceptedRequest(t.Context(), req); err != nil || !found || !reflect.DeepEqual(accepted, old) {
		t.Fatal(old, found, err)
	}
}
