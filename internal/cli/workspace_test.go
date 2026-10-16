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
		if hint.Name == "model.connections" {
			if !strings.Contains(hint.Usage, "[--agent AGENT]") || !strings.Contains(hint.Guidance, "--agent AGENT") || !strings.Contains(hint.Guidance, "same --agent") {
				t.Fatal("model hint omits Agent scope", hint)
			}
			for _, name := range []string{"connections", "list"} {
				modelCmd, _, err := root.Find([]string{"model", name})
				if err != nil || modelCmd.Flags().Lookup("agent") == nil {
					t.Fatal("hint advertises an unregistered flag", name, err)
				}
			}
		}
	}
}

type workspaceQueryEcho struct{}

func (workspaceQueryEcho) Handle(_ context.Context, req protocol.Request) (any, error) {
	return req, nil
}

func (workspaceQueryEcho) Subscribe(context.Context, protocol.Request, func(protocol.Event) error) error {
	return protocol.E("unsupported", "fixture only accepts queries")
}

func TestWorkspaceModelCLIForwardsAgentScope(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-model-query-")
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
	server, err := protocol.Listen(paths.Socket, workspaceQueryEcho{})
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
	for _, tc := range []struct {
		name, method string
		args         []string
		params       string
	}{
		{"connections default", "model.connections", []string{"model", "connections"}, `{"limit":20}`},
		{"models default", "model.list", []string{"model", "list", "configured"}, `{"connection":"configured","limit":20}`},
		{"connections scoped", "model.connections", []string{"model", "connections", "--agent", "agent-one", "--after", "previous", "--limit", "3", "--version", "catalog-one"}, `{"agent_id":"agent-one","after":"previous","limit":3,"version":"catalog-one"}`},
		{"models scoped", "model.list", []string{"model", "list", "--agent", "agent-two", "plugin:resource:provider", "--after", "previous-model", "--limit", "7", "--version", "catalog-two"}, `{"agent_id":"agent-two","connection":"plugin:resource:provider","after":"previous-model","limit":7,"version":"catalog-two"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			code := Run(ctx, append([]string{"--profile", "queries"}, tc.args...), nil, &out, &diagnostic, BuildInfo{})
			if code != 0 || diagnostic.Len() != 0 {
				t.Fatal(code, diagnostic.String())
			}
			var req protocol.Request
			if err := json.Unmarshal(out.Bytes(), &req); err != nil || req.Method != tc.method || req.OperationID != "" {
				t.Fatal("incorrect read request", req, err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(req.Params, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.params), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("query = %v; want %v", got, want)
			}
		})
	}
}
