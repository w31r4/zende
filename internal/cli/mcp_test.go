package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestMCPCLIRegistersAndSearchesOverIPC(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-mcp-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	paths, err := (&app{profile: "mcp"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	fixture := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SupportedProtocolVersions: []string{"2025-11-25"}})
	fixture.AddTool(&mcpsdk.Tool{Name: "test_tool", Description: "Local search fixture", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		t.Error("search invoked a tool")
		return nil, nil
	})
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return fixture }, nil))
	defer httpServer.Close()
	defer core.CloseResources()
	ipc, err := protocol.Listen(paths.Socket, core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ipc.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	run := func(args ...string) []byte {
		t.Helper()
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "mcp"}, args...), nil, &out, &diagnostics, BuildInfo{})
		if code != 0 || diagnostics.Len() != 0 {
			t.Fatal(args, code, diagnostics.String())
		}
		return out.Bytes()
	}
	var agent protocol.Accepted
	if err := json.Unmarshal(run("agent", "create", "--cwd", dir), &agent); err != nil {
		t.Fatal(err)
	}
	config := resource.MCPConfig{SchemaVersion: 1, Name: "fixture", Transport: "streamable-http", URL: httpServer.URL}
	b, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	var registered protocol.Accepted
	if err := json.Unmarshal(run("--request-id", "install", "resource", "register", agent.AgentID, "mcp.json", "--kind", "mcp"), &registered); err != nil || registered.Resource == nil {
		t.Fatal(registered, err)
	}
	var result mcp.SearchResult
	if err := json.Unmarshal(run("mcp", "search", agent.AgentID, registered.Resource.ID, "--query", "fixture", "--limit", "1", "--max-bytes", "1024"), &result); err != nil || len(result.Tools) != 1 || result.Generation == 0 || result.ResourceVersion != registered.Resource.Version {
		t.Fatal(result, err)
	}
	var out, diagnostics bytes.Buffer
	code := Run(ctx, []string{"--profile", "mcp", "mcp", "search", agent.AgentID, registered.Resource.ID, "--max-bytes", "1"}, nil, &out, &diagnostics, BuildInfo{})
	var failure protocol.Error
	if code != 2 || out.Len() != 0 || json.Unmarshal(diagnostics.Bytes(), &failure) != nil || failure.Data.Kind != "invalid_params" {
		t.Fatal(code, out.String(), diagnostics.String())
	}
	for _, args := range [][]string{{"mcp", "search", "--help"}, {"resource", "register", "--help"}} {
		if len(run(args...)) == 0 {
			t.Fatal("empty help")
		}
	}
}
