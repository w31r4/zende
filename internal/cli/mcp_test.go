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
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
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
	var calls atomic.Int32
	fixture.AddTool(&mcpsdk.Tool{Name: "test_tool", Description: "Local search fixture", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "saved response"}}}, nil
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
	if calls.Load() != 0 {
		t.Fatal("search invoked a tool")
	}
	input := protocol.Submit{AgentID: agent.AgentID, Input: "invoke"}
	req, _ := protocol.NewRequest("agent.submit", "submit", input)
	accepted, err := s.Submit(ctx, req, input, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, agent.AgentID); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(protocol.MCPInvoke{ServerID: registered.Resource.ID, ToolRef: result.Tools[0].Ref, Arguments: json.RawMessage(`{}`)})
	call := tool.Call{ID: "original-mcp", AgentID: agent.AgentID, TaskID: accepted.TaskID, Arguments: args}
	if err := s.AppendTaskEvent(ctx, agent.AgentID, accepted.TaskID, "tool.dispatching", map[string]any{"operation_id": call.ID, "call": model.ToolCall{ID: "model-call", Name: "tool_invoke", Arguments: args}}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.MCPExecutors()[1].Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	core.CloseResources()
	var receipt protocol.MCPCallReceipt
	if err := json.Unmarshal(run("mcp", "inspect", agent.AgentID, call.ID), &receipt); err != nil || receipt.State != "completed" {
		t.Fatal(receipt, err)
	}
	var page protocol.MCPResultPage
	if err := json.Unmarshal(run("mcp", "result", agent.AgentID, call.ID, "--part", "0", "--limit", "4"), &page); err != nil || page.Text != "save" || page.Next != 4 || !page.More {
		t.Fatal(page, err)
	}
	if err := json.Unmarshal(run("mcp", "result", agent.AgentID, call.ID, "--part", "0", "--offset", "4"), &page); err != nil || page.Text != "d response" || page.More {
		t.Fatal(page, err)
	}
	for _, args := range [][]string{{"mcp", "inspect", agent.AgentID, "missing"}, {"mcp", "result", agent.AgentID, call.ID, "--part", "wrong"}} {
		out.Reset()
		diagnostics.Reset()
		if code := Run(ctx, append([]string{"--profile", "mcp"}, args...), nil, &out, &diagnostics, BuildInfo{}); code == 0 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"kind"`) {
			t.Fatal(code, out.String(), diagnostics.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("read-only commands invoked again", calls.Load())
	}
	for _, args := range [][]string{{"mcp", "search", "--help"}, {"resource", "register", "--help"}, {"mcp", "inspect", "--help"}, {"mcp", "result", "--help"}} {
		if len(run(args...)) == 0 {
			t.Fatal("empty help")
		}
	}
}
