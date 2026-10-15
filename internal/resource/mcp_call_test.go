package resource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
)

func TestMCPCallUsesPinnedHTTPHeadersAfterPartialRefresh(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2026-07-28"}, Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	old := &mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object","properties":{"tenant":{"type":"string","x-mcp-header":"Old"}},"required":["tenant"]}`)}
	server.AddTool(old, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
	})
	var poison atomic.Bool
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" && poison.Load() {
				if req.GetParams().(*mcpsdk.ListToolsParams).Cursor != "" {
					return nil, errors.New("fixture partial failure")
				}
				changed := *old
				changed.InputSchema = json.RawMessage(`{"type":"object","properties":{"tenant":{"type":"string","x-mcp-header":"New"}},"required":["tenant"]}`)
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{&changed}, NextCursor: "broken"}, nil
			}
			return next(ctx, method, req)
		}
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true})
	var calls atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") == "tools/call" {
			calls.Add(1)
			if r.Header.Get("Mcp-Param-Old") != "tenant" || r.Header.Get("Mcp-Param-New") != "" {
				t.Error("call used another definition's headers", r.Header.Get("Mcp-Param-Old"), r.Header.Get("Mcp-Param-New"))
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	id, _ := Identity("mcp", filepath.Join(t.TempDir(), "mcp.json"))
	conn, err := ConnectMCP(t.Context(), id, MCPConfig{SchemaVersion: 1, Name: "fixture", Transport: "streamable-http", URL: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	snapshot, _ := conn.Snapshot()
	page, _ := snapshot.Search(mcp.SearchRequest{})
	ref := page.Tools[0].Ref
	poison.Store(true)
	if err := conn.Refresh(t.Context()); err == nil {
		t.Fatal("partial refresh accepted")
	}
	call, release, err := conn.LeaseCall(t.Context(), ref, []byte(`{"tenant":"tenant"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result, err := call.Invoke(t.Context())
	if err != nil || !strings.Contains(string(result.Result), "ok") || calls.Load() != 1 {
		t.Fatal(result, err, calls.Load())
	}
}

func TestMCPLeaseRetirementAndForceClose(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "shutdown"}[force], func(t *testing.T) {
			server := resourceMCPFixture()
			started, finish := make(chan struct{}), make(chan struct{})
			server.AddTool(&mcpsdk.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				close(started)
				select {
				case <-finish:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "complete"}}}, nil
			})
			httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
			defer httpServer.Close()
			var finishOnce sync.Once
			unblock := func() { finishOnce.Do(func() { close(finish) }) }
			defer unblock()
			id, _ := Identity("mcp", filepath.Join(t.TempDir(), "mcp.json"))
			conn, err := ConnectMCP(t.Context(), id, MCPConfig{SchemaVersion: 1, Name: "fixture", Transport: "streamable-http", URL: httpServer.URL}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			snapshot, _ := conn.Snapshot()
			page, _ := snapshot.Search(mcp.SearchRequest{})
			ref := page.Tools[0].Ref
			call, release, err := conn.LeaseCall(t.Context(), ref, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			done := make(chan error, 1)
			go func() { _, err := call.Invoke(t.Context()); release(); release(); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("no dispatch")
			}
			conn.Retire()
			if _, _, err := conn.LeaseCall(t.Context(), ref, []byte(`{}`)); err == nil {
				t.Fatal("retired connection accepted new lease")
			}
			select {
			case <-conn.Done():
				t.Fatal("retirement cancelled lease")
			default:
			}
			if force {
				// A peer can drop session deletion during cancellation. The local
				// connection must still close and the effect stays unknown.
				_ = conn.Close()
			} else {
				unblock()
			}
			select {
			case err := <-done:
				if force {
					var e *protocol.Error
					if !errors.As(err, &e) || e.Data.Kind != "operation_unknown" {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lease did not finish")
			}
			select {
			case <-conn.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("retired connection was not reclaimed")
			}
		})
	}
}
