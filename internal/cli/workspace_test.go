package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestWorkspaceCLIQueriesKeepSessionAndContextUnchanged(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-workspace-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	paths, err := (&app{profile: "queries"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := resource.Defaults(dir)
	// The catalog is usable even without a selected default/running model loop.
	cfg.Connections["first"] = resource.Connection{Vendor: "qwen", Protocol: "responses", BaseURL: "https://one.invalid", Credential: "env:UNSET_TEST_KEY", Models: []resource.ModelConfig{{ID: "model", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"low", "high"}}}}
	cfg.Connections["second"] = cfg.Connections["first"]
	c := runtime.New(s, resource.Effective{Config: cfg})
	c.Catalog, err = provider.New(cfg, func(string) string { t.Error("read-only CLI resolved credentials"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	run := func(args ...string) []byte {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "queries"}, args...), nil, &out, &diagnostic, BuildInfo{})
		if code != 0 || diagnostic.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostic.String())
		}
		return out.Bytes()
	}
	var created protocol.Accepted
	if err := json.Unmarshal(run("agent", "create", "--cwd", dir, "preserved input"), &created); err != nil {
		t.Fatal(err)
	}
	before := run("agent", "inspect", created.AgentID)
	history := run("agent", "history", created.AgentID)
	if err := os.Mkdir(filepath.Join(dir, "中文"), 0700); err != nil {
		t.Fatal(err)
	}
	var page protocol.DirectoryPage
	if err := json.Unmarshal(run("workspace", "directory", created.AgentID, "中文"), &page); err != nil || page.Path != filepath.Join(dir, "中文") || page.Parent != dir || len(page.Entries) != 0 {
		t.Fatal(page, err)
	}
	var status protocol.GitStatus
	if err := json.Unmarshal(run("workspace", "git", created.AgentID, "中文"), &status); err != nil || status.State != "none" {
		t.Fatal(status, err)
	}
	var connections protocol.ConnectionPage
	if err := json.Unmarshal(run("model", "connections", "--limit", "1"), &connections); err != nil || len(connections.Items) != 1 || connections.Items[0].ID != "first" || connections.Next != "first" {
		t.Fatal(connections, err)
	}
	version := connections.Version
	next := connections.Next
	connections = protocol.ConnectionPage{}
	if err := json.Unmarshal(run("model", "connections", "--after", next, "--version", version), &connections); err != nil || len(connections.Items) != 1 || connections.Items[0].ID != "second" || connections.Next != "" {
		t.Fatal(connections, err)
	}
	var models protocol.ModelPage
	if err := json.Unmarshal(run("model", "list", "first", "--version", version), &models); err != nil || len(models.Items) != 1 || !reflect.DeepEqual(models.Items[0].Efforts, []string{"low", "high"}) {
		t.Fatal(models, err)
	}
	for _, args := range [][]string{{"workspace", "directory"}, {"workspace", "git"}, {"model", "connections"}, {"model", "list"}} {
		if !bytes.Contains(run(append(args, "--help")...), []byte("Usage:")) {
			t.Fatal("missing help", args)
		}
	}
	for _, args := range [][]string{
		{"workspace", "directory", created.AgentID, "missing"},
		{"workspace", "directory", "missing", "/"},
		{"workspace", "git", "missing", "/"},
		{"model", "connections", "--version", "stale"},
		{"model", "list", "unconfigured"},
		{"model", "list", "first", "--limit", "101"},
	} {
		var out, diagnostic bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "queries"}, args...), nil, &out, &diagnostic, BuildInfo{}); code == 0 || out.Len() != 0 || diagnostic.Len() == 0 {
			t.Fatal("failure was not explicit", args, code, out.String(), diagnostic.String())
		}
	}
	for _, method := range []string{"workspace.directory", "workspace.git", "model.connections", "model.list"} {
		req, _ := protocol.NewRequest(method, "", map[string]string{"unknown": "field"})
		if _, err := c.Handle(ctx, req); err == nil {
			t.Fatal("unknown fields accepted", method)
		}
	}
	if !bytes.Equal(before, run("agent", "inspect", created.AgentID)) || !bytes.Equal(history, run("agent", "history", created.AgentID)) {
		t.Fatal("browsing changed cwd, model, task or history")
	}
}

func TestWorkspacePromptHintsMatchCLI(t *testing.T) {
	root := (&app{}).command()
	for _, hint := range workspaceHints("/binary path/zende", "bound-profile") {
		parts := strings.Split(hint.Name, ".")
		cmd, remaining, err := root.Find(parts)
		if err != nil || len(remaining) != 0 || cmd.Name() != parts[1] || cmd.RunE == nil {
			t.Fatal(hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "'/binary path/zende' --profile 'bound-profile'") {
			t.Fatal(hint.Usage)
		}
	}
}
