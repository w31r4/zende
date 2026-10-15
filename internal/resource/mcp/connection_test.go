package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixtureServer(version string) *mcpsdk.Server {
	return mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{
		SupportedProtocolVersions: []string{version}, PageSize: 1,
		Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{ListChanged: true}},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		SetCacheable: func(_ context.Context, _ mcpsdk.Request, c *mcpsdk.Cacheable) { c.TTLMs = 60_000 },
	})
}

func addTool(s *mcpsdk.Server, value *mcpsdk.Tool) {
	s.AddTool(value, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fixture"}}}, nil
	})
}

func connectMemory(t *testing.T, server *mcpsdk.Server) *Connection {
	t.Helper()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	session, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	c, err := Connect(t.Context(), "fixture", "v1", clientTransport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func waitCatalog(t *testing.T, c *Connection, predicate func(Status) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if predicate(c.Status()) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("catalog did not update: %+v", c.Status())
		case <-c.Done():
			t.Fatalf("connection closed: %+v", c.Status())
		case <-c.Updates():
		}
	}
}

func TestMCPHTTPNegotiationPaginationAndHotCatalog(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			server := fixtureServer(version)
			addTool(server, tool("a"))
			addTool(server, tool("b"))
			var lists atomic.Int32
			server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
				return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
					if method == "tools/list" {
						lists.Add(1)
					}
					if method == "initialize" {
						caps := req.GetParams().(*mcpsdk.InitializeParams).Capabilities
						if caps.RootsV2 != nil || caps.Sampling != nil || caps.Elicitation != nil {
							t.Error("unimplemented client capability advertised")
						}
					}
					return next(ctx, method, req)
				}
			})
			httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: version == "2026-07-28"}))
			defer httpServer.Close()
			transport := &mcpsdk.StreamableClientTransport{Endpoint: httpServer.URL, HTTPClient: httpServer.Client(), MaxEventSize: 1 << 20, MaxRetries: -1}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c, err := Connect(ctx, "http", "config-v1", transport)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			cancel() // initial request cancellation must not detach the live server
			if got := c.session.InitializeResult().ProtocolVersion; got != version {
				t.Fatalf("protocol %s", got)
			}
			initial, err := c.Snapshot()
			if err != nil || initial.Len() != 2 || lists.Load() < 2 {
				t.Fatalf("catalog %v error %v lists %d", initial, err, lists.Load())
			}
			before := lists.Load()
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if lists.Load()-before < 2 {
				t.Fatal("explicit refresh served cached pages")
			}
			addTool(server, tool("c"))
			waitCatalog(t, c, func(s Status) bool { return s.State == "ready" && s.Tools == 3 })
			current, _ := c.Snapshot()
			if current.Version() == initial.Version() || initial.Len() != 2 {
				t.Fatal("snapshot mutation/version failure")
			}
			search, _ := initial.Search(SearchRequest{})
			if _, err := current.Lookup(search.Tools[0].Ref); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Snapshot(); err == nil || c.Status().State != "closed" {
				t.Fatal("closed connection remained ready")
			}
		})
	}
}

func TestMCPFailedRefreshRetainsCompleteCatalogAndSanitizesErrors(t *testing.T) {
	server := fixtureServer("2026-07-28")
	addTool(server, tool("a"))
	var mu sync.Mutex
	var override listTools
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			mu.Lock()
			fn := override
			mu.Unlock()
			if method == "tools/list" && fn != nil {
				return fn(ctx, req.GetParams().(*mcpsdk.ListToolsParams).Cursor)
			}
			return next(ctx, method, req)
		}
	})
	c := connectMemory(t, server)
	initial, _ := c.Snapshot()
	tests := map[string]listTools{
		"nil tool": func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("a"), nil}}, nil
		},
		"header annotation filtered": func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			value := tool("bad")
			value.InputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"bad\nheader"}}}`)
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("a"), value}}, nil
		},
		"second page fails": func(_ context.Context, cursor string) (*mcpsdk.ListToolsResult, error) {
			if cursor == "" {
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("new")}, NextCursor: "next"}, nil
			}
			return nil, errors.New("DO_NOT_LEAK_CREDENTIAL")
		},
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			override = fn
			mu.Unlock()
			err := c.Refresh(t.Context())
			if err == nil || strings.Contains(err.Error(), "DO_NOT_LEAK_CREDENTIAL") {
				t.Fatalf("diagnostic %v", err)
			}
			current, err := c.Snapshot()
			if err != nil || current != initial || c.Status().State != "ready" || c.Status().Error == nil {
				t.Fatalf("lost old catalog: %+v %v", c.Status(), err)
			}
			b, _ := json.Marshal(c.Status())
			if strings.Contains(string(b), "DO_NOT_LEAK_CREDENTIAL") {
				t.Fatal("error leaked into status")
			}
		})
	}
	mu.Lock()
	override = nil
	mu.Unlock()
	if err := c.Refresh(t.Context()); err != nil || c.Status().Error != nil {
		t.Fatalf("recovery %v %+v", err, c.Status())
	}
}

func TestMCPRefreshCancellationAndConcurrentReaders(t *testing.T) {
	server := fixtureServer("2025-11-25")
	addTool(server, tool("a"))
	started := make(chan struct{}, 1)
	var block atomic.Bool
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" && block.Load() {
				signal(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, method, req)
		}
	})
	c := connectMemory(t, server)
	initial, _ := c.Snapshot()
	block.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Refresh(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	if s, err := c.Snapshot(); err != nil || s != initial {
		t.Fatal("refresh hid valid catalog", err)
	}
	queuedCtx, queuedCancel := context.WithCancel(t.Context())
	queuedCancel()
	errorKind(t, c.Refresh(queuedCtx), "cancelled")
	cancel()
	select {
	case err := <-done:
		errorKind(t, err, "cancelled")
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled refresh hung")
	}
	block.Store(false)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				if err := c.Refresh(t.Context()); err != nil {
					t.Error(err)
					return
				}
				s, err := c.Snapshot()
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Search(SearchRequest{}); err != nil {
					t.Error(err)
				}
				_ = c.Status()
			}
		})
	}
	wg.Wait()
	if c.Status().Error != nil {
		t.Fatal(c.Status())
	}
}

func TestMCPFailedConnectClosesTransport(t *testing.T) {
	server := fixtureServer("2026-07-28")
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" {
				return nil, &jsonrpc.Error{Code: -32603, Message: "DO_NOT_LEAK_CREDENTIAL"}
			}
			return next(ctx, method, req)
		}
	})
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	_, err = Connect(t.Context(), "s", "v", clientTransport)
	errorKind(t, err, "unavailable")
	if strings.Contains(err.Error(), "DO_NOT_LEAK_CREDENTIAL") {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = ss.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed connect leaked transport")
	}
}

type transportFunc func(context.Context) (mcpsdk.Connection, error)

func (f transportFunc) Connect(ctx context.Context) (mcpsdk.Connection, error) { return f(ctx) }

type stalledListenConnection struct{ mcpsdk.Connection }

func (c stalledListenConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "subscriptions/listen" {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.Connection.Write(ctx, msg)
}

func TestMCPPreparationDeadlineIncludesSubscriptionWrite(t *testing.T) {
	server := fixtureServer("2026-07-28")
	ct, st := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	transport := transportFunc(func(ctx context.Context) (mcpsdk.Connection, error) {
		conn, err := ct.Connect(ctx)
		return stalledListenConnection{conn}, err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Connect(ctx, "s", "v", transport); done <- err }()
	select {
	case err := <-done:
		errorKind(t, err, "timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("subscription write escaped preparation deadline")
	}
}

func TestMCPConnectAdmissionAndFailureCleanup(t *testing.T) {
	var started bool
	neverStart := transportFunc(func(context.Context) (mcpsdk.Connection, error) {
		started = true
		return nil, errors.New("DO_NOT_LEAK_CREDENTIAL")
	})
	for _, id := range []string{"", "bad/id", strings.Repeat("x", 129)} {
		_, err := Connect(t.Context(), id, "v", neverStart)
		errorKind(t, err, "invalid_params")
	}
	if started {
		t.Fatal("invalid identity started transport")
	}
	_, err := Connect(t.Context(), "s", "v", neverStart)
	errorKind(t, err, "unavailable")
	if strings.Contains(err.Error(), "DO_NOT_LEAK_CREDENTIAL") {
		t.Fatal(err)
	}
	for _, rejectInitialize := range []bool{true, false} {
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{
			SupportedProtocolVersions: []string{"2025-11-25"},
			Capabilities:              &mcpsdk.ServerCapabilities{},
			Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
			return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
				if rejectInitialize && method == "initialize" {
					return nil, errors.New("DO_NOT_LEAK_CREDENTIAL")
				}
				return next(ctx, method, req)
			}
		})
		ct, st := mcpsdk.NewInMemoryTransports()
		ss, err := server.Connect(t.Context(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ss.Close() })
		_, err = Connect(t.Context(), "s", "v", ct)
		if rejectInitialize {
			errorKind(t, err, "unavailable")
		} else {
			errorKind(t, err, "invalid_resource")
		}
		done := make(chan struct{})
		go func() { _ = ss.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("preparation failure leaked session")
		}
	}
}

// A real subprocess over newline-delimited stdin/stdout; never a model server.
func TestMCPStdioFixture(t *testing.T) {
	if os.Getenv("ZENDE_MCP_TEST_FIXTURE") != "1" {
		return
	}
	server := fixtureServer("2026-07-28")
	addTool(server, tool("stdio"))
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{MaxLineLength: 1 << 20}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestMCPStdioLifetimeAndCrash(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "crash"}[crash], func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMCPStdioFixture$")
			cmd.Env = append(os.Environ(), "ZENDE_MCP_TEST_FIXTURE=1")
			c, err := Connect(t.Context(), "stdio", "v", &mcpsdk.CommandTransport{Command: cmd, TerminateDuration: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if s, err := c.Snapshot(); err != nil || s.Len() != 1 {
				t.Fatalf("%v %v", s, err)
			}
			if crash {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-c.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("crash not detected")
				}
				if c.Status().State != "unavailable" {
					t.Fatal(c.Status())
				}
				if _, err := c.Snapshot(); err == nil {
					t.Fatal("crashed server advertised ready")
				}
			}
			_ = c.Close() // a killed subprocess may return a transport-close error
			if cmd.ProcessState == nil {
				t.Fatal("child not reaped")
			}
		})
	}
}
