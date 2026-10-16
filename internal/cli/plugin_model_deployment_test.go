package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestPluginModelThroughBinaryCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-model-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	t.Setenv(protocol.CallerEnvironment, "")
	t.Setenv("ZENDE_TEST_PLUGIN", "1")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_AUDIT", "")
	paths, err := (&app{profile: "plugin-model"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	defer c.CloseResources()
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	served, scheduled := make(chan error, 1), make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	go func() { scheduled <- c.Run(ctx) }()
	defer func() {
		cancel()
		for _, done := range []<-chan error{served, scheduled} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("fixture core did not stop")
			}
		}
	}()
	run := func(result any, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "plugin-model"}, args...)...)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		if err := cmd.Run(); err != nil || diagnostic.Len() != 0 || json.Unmarshal(out.Bytes(), result) != nil {
			t.Fatal(args, err, out.String(), diagnostic.String())
		}
	}
	var agent, published protocol.Accepted
	run(&agent, "agent", "create", "--cwd", dir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := plugin.Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", ProtocolVersions: []int{1}, Command: exe, Args: []string{"-test.run=^TestCLIPluginProcessHelper$"}, Capabilities: []string{"providers"}}
	raw, _ := json.Marshal(manifest)
	source := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run(&published, "resource", "register", agent.AgentID, source, "--kind", "plugin")
	connection := "plugin:" + published.Resource.ID + ":fixture-models"
	var connections protocol.ConnectionPage
	run(&connections, "model", "connections", "--agent", agent.AgentID)
	if len(connections.Items) != 1 || connections.Items[0].ID != connection {
		t.Fatal(connections)
	}
	var models protocol.ModelPage
	run(&models, "model", "list", connection, "--agent", agent.AgentID, "--version", connections.Version)
	if len(models.Items) != 1 || models.Items[0].ID != "fixture-text" {
		t.Fatal(models)
	}
	var selected, work protocol.Accepted
	run(&selected, "agent", "select", agent.AgentID, "--model", connection+"/fixture-text", "--effort", "high", "--catalog-version", models.Version)
	run(&work, "agent", "submit", agent.AgentID, "answer through plugin")
	wait := func(work protocol.Accepted) {
		for {
			var snapshot protocol.Snapshot
			run(&snapshot, "agent", "inspect", agent.AgentID)
			for _, task := range snapshot.Tasks {
				if task.ID != work.TaskID {
					continue
				}
				if task.State == "completed" {
					if !strings.Contains(task.Result, `"version":"1"`) || !strings.Contains(task.Result, `"effort":"high"`) {
						t.Fatal(task)
					}
					return
				}
				if task.State == "failed" || task.State == "unknown" || task.State == "cancelled" {
					t.Fatal(task)
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal("plugin model task did not finish")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(work)
	// Child preparation exceeds the old 10-second generic CLI deadline. The
	// plugin is still within its advertised initialization budget.
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "slow-initialize")
	var child protocol.Accepted
	run(&child, "--request-id", "spawn-slow-model", "agent", "spawn", "--parent", agent.AgentID, "--context", "none", "child answer")
	if child.AgentID == "" || child.AgentID == agent.AgentID || child.TaskID == work.TaskID {
		t.Fatal(child)
	}
	agent = child
	wait(child)
}

func TestPluginModelSpawnTimeout(t *testing.T) {
	if got := requestTimeout("agent.spawn", protocol.SpawnAgent{}); got != plugin.InitializeTimeout+10*time.Second {
		t.Fatal(got)
	}
}
