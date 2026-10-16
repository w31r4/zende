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
	"zende.local/harness/internal/resource/plugin/fixture"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestCLIPluginProcessHelper(t *testing.T) { fixture.RunFromEnv() }

func TestPluginCatalogThroughBinaryCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-plugin-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	t.Setenv("ZENDE_TEST_PLUGIN", "1")
	t.Setenv(protocol.CallerEnvironment, "")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", "")
	audit := filepath.Join(dir, "plugin-audit.jsonl")
	t.Setenv("ZENDE_TEST_PLUGIN_AUDIT", audit)
	paths, err := (&app{profile: "plugins"}).paths()
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
	// The full suite now starts real CLI processes for five capability types,
	// retained/offline reads and help. Individual RPCs keep their own deadlines.
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	run := func(success bool, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "plugins"}, args...)...)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		err := cmd.Run()
		if success {
			if err != nil || diagnostic.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) || !json.Valid(out.Bytes()) {
				t.Fatal(args, err, out.String(), diagnostic.String())
			}
			return out.Bytes()
		}
		exit, ok := err.(*exec.ExitError)
		var failure protocol.Error
		if !ok || out.Len() != 0 || bytes.Contains(diagnostic.Bytes(), []byte{27}) || json.Unmarshal(diagnostic.Bytes(), &failure) != nil {
			t.Fatal(args, err, out.String(), diagnostic.String())
		}
		wantExit := 1
		if failure.Data.Kind == "invalid_params" || failure.Data.Kind == "invalid_request" {
			wantExit = 2
		}
		if exit.ExitCode() != wantExit {
			t.Fatal(args, err, "want exit", wantExit, diagnostic.String())
		}
		return diagnostic.Bytes()
	}
	var agent protocol.Accepted
	if err := json.Unmarshal(run(true, "agent", "create", "--cwd", dir), &agent); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := plugin.Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", ProtocolVersions: []int{1}, Command: exe, Args: []string{"-test.run=^TestCLIPluginProcessHelper$"}, Capabilities: []string{"tools", "commands", "providers", "resources", "events"}}
	source := filepath.Join(dir, "中文 plugin.json")
	write := func() {
		t.Helper()
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	register := []string{"--request-id", "register-plugin", "resource", "register", agent.AgentID, filepath.Base(source), "--kind", "plugin"}
	first := run(true, register...)
	var accepted protocol.Accepted
	if err := json.Unmarshal(first, &accepted); err != nil || accepted.Resource == nil {
		t.Fatal(accepted, err)
	}
	id := accepted.Resource.ID
	var page protocol.ResourcePage
	if err := json.Unmarshal(run(true, "resource", "list", agent.AgentID, "--kind", "plugin"), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != id {
		t.Fatal(page, err)
	}
	var catalog resource.PluginCatalog
	if err := json.Unmarshal(run(true, "plugin", "catalog", agent.AgentID, id), &catalog); err != nil || catalog.Version != accepted.Resource.Version || len(catalog.Definitions) != 2 || catalog.Definitions[0].Kind != "command" || catalog.Definitions[1].Kind != "tool" {
		t.Fatal(catalog, err)
	}
	if len(catalog.Providers) != 1 || len(catalog.Resources) != 2 || catalog.Providers[0].Models[0].ID != "fixture-text" || catalog.Resources[0].Kind != "prompt" {
		t.Fatal("extended plugin definitions missing from retained CLI catalog", catalog)
	}
	var status protocol.ResourceStatus
	if err := json.Unmarshal(run(true, "resource", "status", agent.AgentID, id), &status); err != nil || status.State != "ready" {
		t.Fatal("CLI exit stopped plugin", status, err)
	}
	before, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var instances runtime.PluginInstances
		if err := json.Unmarshal(run(true, "plugin", "instances", agent.AgentID, id), &instances); err != nil || instances.AgentID != agent.AgentID || instances.ResourceID != id || len(instances.Items) != 1 || instances.Current == "" || instances.MaxRetiring != runtime.MaxRetiringPluginInstances || instances.MaxActiveCalls != runtime.MaxActivePluginCalls {
			t.Fatal(instances, err)
		}
		item := instances.Items[0]
		if item.Instance.InstanceID != instances.Current || item.Instance.State != "ready" || item.ResourceVersion != accepted.Resource.Version || item.Generation != accepted.ResourceGeneration || item.References != 0 || item.Retiring {
			t.Fatal(instances)
		}
	}
	if !bytes.Equal(first, run(true, register...)) {
		t.Fatal("repeated registration changed receipt")
	}
	after, _ := os.ReadFile(audit)
	if !bytes.Equal(before, after) {
		t.Fatal("read/replay caused plugin RPC")
	}
	var children []protocol.Accepted
	for _, kind := range []string{"skill", "prompt"} {
		var a protocol.Accepted
		args := []string{"--request-id", "install-" + kind, "resource", "register", agent.AgentID, "plugin://" + id + "/" + kind + "/fixture-" + kind, "--kind", kind}
		first := run(true, args...)
		if err := json.Unmarshal(first, &a); err != nil || a.Resource == nil || a.Resource.PluginOrigin == nil || a.Resource.PluginOrigin.Plugin.Version != accepted.Resource.Version {
			t.Fatal(a, err)
		}
		before, _ := os.ReadFile(audit)
		if !bytes.Equal(first, run(true, args...)) {
			t.Fatal("child receipt changed")
		}
		after, _ := os.ReadFile(audit)
		if !bytes.Equal(before, after) {
			t.Fatal("child retry reread plugin")
		}
		children = append(children, a)
	}
	manifest.Version = "2"
	write()
	var updated protocol.Accepted
	if err := json.Unmarshal(run(true, "--request-id", "refresh-plugin", "resource", "refresh", agent.AgentID, id), &updated); err != nil || updated.Resource.Version == accepted.Resource.Version {
		t.Fatal(updated, err)
	}
	// Compare the full decoded retained definition, not only its display name.
	raw, _ := json.Marshal(catalog)
	if !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(run(true, "plugin", "catalog", agent.AgentID, id, "--version", catalog.Version))) {
		t.Fatal("historical catalog changed")
	}
	var failure protocol.Error
	if err := json.Unmarshal(run(false, "plugin", "catalog", agent.AgentID, id, "--version", "latest"), &failure); err != nil || failure.Data.Kind != "invalid_params" {
		t.Fatal(failure, err)
	}
	checkPluginCLICommands(t, ctx, s, binary, source, run)
	checkPluginCLIEvents(t, ctx, s, source, run)
	retainedCalls := seedPluginCLIReads(t, ctx, s, source, updated, run)
	c.CloseResources()
	before, _ = os.ReadFile(audit)
	if err := json.Unmarshal(run(true, "resource", "status", agent.AgentID, id), &status); err != nil || status.State != "unavailable" {
		t.Fatal(status, err)
	}
	run(true, "plugin", "catalog", agent.AgentID, id)
	var offline runtime.PluginInstances
	if err := json.Unmarshal(run(true, "plugin", "instances", agent.AgentID, id), &offline); err != nil || len(offline.Items) != 0 || offline.Current != "" {
		t.Fatal(offline, err)
	}
	checkPluginCLIReads(t, retainedCalls, run)
	for _, a := range children {
		var content protocol.ResourceContent
		if err := json.Unmarshal(run(true, "resource", "read", agent.AgentID, a.Resource.ID, "--version", a.Resource.Version), &content); err != nil || content.PluginOrigin == nil || content.PluginOrigin.Plugin.Version != accepted.Resource.Version || !strings.Contains(content.Text, "Read this fixture") {
			t.Fatal(content, err)
		}
		if err := json.Unmarshal(run(true, "resource", "status", agent.AgentID, a.Resource.ID), &status); err != nil || status.State != "ready" {
			t.Fatal(status, err)
		}
	}
	after, _ = os.ReadFile(audit)
	if !bytes.Equal(before, after) {
		t.Fatal("offline read started a process")
	}
	help, err := exec.CommandContext(ctx, binary, "plugin", "catalog", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "--version") || !strings.Contains(string(help), "does not start or invoke") {
		t.Fatal(string(help), err)
	}
	help, err = exec.CommandContext(ctx, binary, "plugin", "instances", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "does not start a process") {
		t.Fatal(string(help), err)
	}
	if !protocol.ReadOnly("plugin.catalog") || !protocol.ReadOnly("plugin.instances") {
		t.Fatal("plugin reads classified as effects")
	}
	help, err = exec.CommandContext(ctx, binary, "resource", "register", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "plugin://RESOURCE/skill/NAME") {
		t.Fatal(string(help), err)
	}
	for _, command := range []string{"inspect", "progress", "result"} {
		help, err := exec.CommandContext(ctx, binary, "plugin", command, "--help").CombinedOutput()
		if err != nil || !strings.Contains(string(help), command+" AGENT OPERATION") || !protocol.ReadOnly("plugin."+command) {
			t.Fatal(command, string(help), err)
		}
	}
}
