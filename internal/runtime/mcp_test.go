package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func TestMCPRegisterSearchHotCatalogAndReconnect(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	defer c.CloseResources()
	create := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", "agent", create)
	a, err := s.Create(ctx, req, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}, PageSize: 1, Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{ListChanged: true}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	add := func(name string) {
		server.AddTool(&mcpsdk.Tool{Name: name, Description: "searchable marker", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			t.Error("search executed a tool")
			return &mcpsdk.CallToolResult{}, nil
		})
	}
	add("one")
	add("two")
	var requests atomic.Int32
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); handler.ServeHTTP(w, r) }))
	defer httpServer.Close()
	defer c.CloseResources()
	config := resource.MCPConfig{SchemaVersion: 1, Name: "local", Transport: "streamable-http", URL: httpServer.URL}
	source := filepath.Join(dir, "mcp.json")
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(config)
	write(data)
	input := protocol.RegisterResource{AgentID: a.AgentID, Kind: "mcp", Path: "mcp.json"}
	register, _ := protocol.NewRequest("resource.register", "register", input)
	value, err := c.Handle(ctx, register)
	if err != nil {
		t.Fatal(err)
	}
	accepted := value.(protocol.Accepted)
	if accepted.Resource == nil || accepted.ResourceGeneration == 0 {
		t.Fatal(value)
	}
	target := protocol.ResourceTarget{AgentID: a.AgentID, ID: accepted.Resource.ID}
	status, err := c.resourceStatus(ctx, target)
	if err != nil || status.State != "ready" || status.Update.State != "ready" {
		t.Fatal(status, err)
	}
	page, err := s.Resources(ctx, protocol.ResourceList{AgentID: a.AgentID, Kind: "mcp"})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	contextSnapshot, err := s.ContextSnapshot(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	commands := []CommandHint{{Name: "resource.list"}, {Name: "resource.read"}, {Name: "mcp.search", Usage: "mcp search AGENT RESOURCE"}}
	notice := resourceNotice(contextSnapshot.Resources, 0, false, commands)
	if notice == nil {
		t.Fatal("no published resource notice")
	}
	contextBytes, _ := json.Marshal(contextSnapshot)
	publicPrompt := BuildPrompt(PromptInput{AgentID: a.AgentID, Commands: commands}) + notice.Message.Content[0].Text + string(contextBytes)
	if strings.Contains(publicPrompt, "searchable marker") || strings.Contains(publicPrompt, "inputSchema") {
		t.Fatal("default context expands MCP definitions")
	}
	search := protocol.MCPSearch{AgentID: a.AgentID, ID: target.ID, Query: "marker", Limit: 1}
	result, err := c.searchMCP(ctx, search)
	if err != nil || len(result.Tools) != 1 || result.NextCursor == "" {
		t.Fatal(result, err)
	}
	search.Cursor = result.NextCursor
	next, err := c.searchMCP(ctx, search)
	if err != nil || len(next.Tools) != 1 || next.NextCursor != "" || next.Tools[0].Ref == result.Tools[0].Ref {
		t.Fatal(next, err)
	}
	write([]byte("broken"))
	before := requests.Load()
	replayed, err := c.Handle(ctx, register)
	if err != nil || !reflect.DeepEqual(replayed, accepted) || requests.Load() != before {
		t.Fatal("accepted request reconnected or changed", replayed, err)
	}
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(ctx, refresh); err == nil {
		t.Fatal("broken config published")
	}
	status, err = c.resourceStatus(ctx, target)
	if err != nil || status.State != "ready" || status.Version != accepted.Resource.Version || status.Update.State != "failed" {
		t.Fatal(status, err)
	}
	add("three")
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err = s.ResourceStatus(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if status.Version != accepted.Resource.Version {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notification was not durably published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.searchMCP(ctx, search); err == nil {
		t.Fatal("old cursor survived catalog publication")
	}
	search.Cursor = ""
	search.Limit = 50
	current, err := c.searchMCP(ctx, search)
	if err != nil || len(current.Tools) != 3 {
		t.Fatal(current, err)
	}
	if current.Tools[0].Ref != result.Tools[0].Ref {
		t.Fatal("unrelated addition changed existing ref")
	}
	// Metadata notices do not contain any full tool definition.
	events, err := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events.Events {
		if strings.Contains(string(e.Payload), "inputSchema") {
			t.Fatal("catalog expanded into ordinary history")
		}
	}
	c.CloseResources()
	restarted := New(s, resource.Effective{Config: resource.Defaults(dir)})
	defer restarted.CloseResources()
	status, err = restarted.resourceStatus(ctx, target)
	if err != nil || status.State != "unavailable" || status.Error == nil {
		t.Fatal("saved catalog pretended to be live", status, err)
	}
	// Source is still broken. Recovery uses the published config, not disk edits.
	recovered, err := restarted.searchMCP(ctx, search)
	if err != nil || !reflect.DeepEqual(current, recovered) {
		t.Fatal(recovered, err)
	}
	status, err = restarted.resourceStatus(ctx, target)
	if err != nil || status.State != "ready" {
		t.Fatal(status, err)
	}
	config.Credential = "env:ZENDE_TEST_MISSING_MCP_KEY"
	t.Setenv("ZENDE_TEST_MISSING_MCP_KEY", "")
	data, _ = json.Marshal(config)
	write(data)
	refresh, _ = protocol.NewRequest("resource.refresh", "credential-refresh", target)
	if _, err := restarted.Handle(ctx, refresh); err == nil {
		t.Fatal("missing credential published")
	}
	status, err = restarted.resourceStatus(ctx, target)
	if err != nil || status.State != "ready" || status.Update.Error.Data.Kind != "unavailable" {
		t.Fatal(status, err)
	}
	search.AgentID = "unknown"
	if _, err := restarted.searchMCP(ctx, search); err == nil {
		t.Fatal("cross-agent search accepted")
	}
	search.AgentID = a.AgentID
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := restarted.searchMCP(ctx, search)
			if err != nil {
				var known *protocol.Error
				if !errors.As(err, &known) || known.Data.Kind != "unavailable" {
					t.Error(err)
				}
			}
		}()
	}
	restarted.CloseResources()
	wg.Wait()
}

func TestMCPNoticeNeedsSearchCapability(t *testing.T) {
	info := protocol.ResourceStatus{ResourceInfo: protocol.ResourceInfo{ID: "id", Version: "version", Kind: "mcp", Name: "test"}, Generation: 1}
	snapshot := store.ResourceSnapshot{Catalog: protocol.ResourceCatalog{Generation: 1}, Items: []protocol.ResourceStatus{info}}
	commands := []CommandHint{{Name: "resource.list"}, {Name: "resource.read"}}
	if resourceNotice(snapshot, 0, false, commands) != nil {
		t.Fatal("unusable MCP capability advertised")
	}
	commands = append(commands, CommandHint{Name: "mcp.search"})
	if got := resourceNotice(snapshot, 0, false, commands); got == nil || !strings.Contains(got.Message.Content[0].Text, "mcp.search") {
		t.Fatal(got)
	}
}
