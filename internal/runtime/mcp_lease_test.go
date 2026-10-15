package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func TestMCPPublishedReplacementDrainsLeasedCall(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	create := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", "agent", create)
	a, err := s.Create(t.Context(), req, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	unblock := func() { finishOnce.Do(func() { close(finish) }) }
	fixture := func(name string, wait bool) *httptest.Server {
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: name, Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
		server.AddTool(&mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			if wait {
				close(started)
				select {
				case <-finish:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: name}}}, nil
		})
		return httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	}
	oldServer, newServer := fixture("old", true), fixture("new", false)
	defer oldServer.Close()
	defer newServer.Close()
	defer c.CloseResources()
	defer unblock()
	source := filepath.Join(dir, "mcp.json")
	write := func(url string) {
		t.Helper()
		raw, _ := json.Marshal(resource.MCPConfig{SchemaVersion: 1, Name: "fixture", Transport: "streamable-http", URL: url})
		if err := os.WriteFile(source, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(oldServer.URL)
	reg, _ := protocol.NewRequest("resource.register", "register", protocol.RegisterResource{AgentID: a.AgentID, Kind: "mcp", Path: source})
	v, err := c.Handle(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	target := protocol.ResourceTarget{AgentID: a.AgentID, ID: v.(protocol.Accepted).Resource.ID}
	query := protocol.MCPSearch{AgentID: a.AgentID, ID: target.ID}
	page, err := c.searchMCP(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	oldRef := page.Tools[0].Ref
	slot, err := c.mcpSlot(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldConn := slot.conn
	call, release, err := oldConn.LeaseCall(t.Context(), oldRef, []byte(`{}`))
	slot.unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		result, err := call.Invoke(t.Context())
		if err == nil && !strings.Contains(string(result.Result), "old") {
			t.Error("wrong result", string(result.Result))
		}
		release()
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old call did not start")
	}
	write(newServer.URL)
	refresh, _ := protocol.NewRequest("resource.refresh", "replace", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldConn.Done():
		t.Fatal("publication killed old call")
	default:
	}
	page, err = c.searchMCP(t.Context(), query)
	if err != nil || page.Tools[0].Ref == oldRef {
		t.Fatal(page, err)
	}
	if err := slot.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	newConn := slot.conn
	if _, _, err := newConn.LeaseCall(t.Context(), oldRef, []byte(`{}`)); !isFailure(err, "stale_reference") {
		t.Error("old ref redirected", err)
	}
	newCall, newRelease, err := newConn.LeaseCall(t.Context(), page.Tools[0].Ref, []byte(`{}`))
	slot.unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer newRelease()
	result, err := newCall.Invoke(t.Context())
	newRelease()
	if err != nil || !strings.Contains(string(result.Result), "new") {
		t.Fatal(result, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldConn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("retired connection leaked")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = slot.lock(t.Context())
		remaining := len(slot.retired)
		slot.unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retired slot bookkeeping leaked")
		}
		time.Sleep(time.Millisecond)
	}
}
