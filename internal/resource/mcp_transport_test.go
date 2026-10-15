package resource

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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
)

func resourceMCPFixture() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}, Capabilities: &mcpsdk.ServerCapabilities{Tools: &mcpsdk.ToolCapabilities{ListChanged: true}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s.AddTool(&mcpsdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	return s
}

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("ZENDE_MCP_TEST_HELPER") != "1" {
		return
	}
	_ = resourceMCPFixture().Run(context.Background(), &mcpsdk.StdioTransport{})
	os.Exit(0)
}

func TestMCPManagedStdioAndImmutablePackage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	config := MCPConfig{SchemaVersion: 1, Name: "stdio", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"ZENDE_MCP_TEST_HELPER": "env:ENABLE"}}
	b, _ := json.Marshal(config)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	p, conn, err := PrepareMCP(t.Context(), path, func(string) string { return "1" })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if p.MCP == nil || len(p.MCP.Tools) != 1 || p.Instructions != "" {
		t.Fatal(p.Info)
	}
	copy, err := BuildMCPPackage(p.Info.Source, p.Files, *p.MCP)
	if err != nil || copy.Info.Version != p.Info.Version {
		t.Fatal(copy.Info, err)
	}
	copy.MCP.Tools[0].Tool[0] = '!'
	if _, err := BuildMCPPackage(copy.Info.Source, copy.Files, *copy.MCP); err == nil {
		t.Fatal("corrupted catalog accepted")
	}
	if _, err := BuildMCPPackage(p.Info.Source, p.Files, *p.MCP); err != nil {
		t.Fatal("export aliased source", err)
	}
	content, err := p.Content("")
	if err != nil || content.Text != string(b) {
		t.Fatal(content, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conn.Status().State != "closed" {
		t.Fatal(conn.Status())
	}
}

func TestMCPHTTPReferencesRedirectsAndRedaction(t *testing.T) {
	server := resourceMCPFixture()
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	const secret = "test-secret-value"
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Key") != secret {
			t.Error("missing resolved credentials")
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	c := MCPConfig{SchemaVersion: 1, Name: "remote", Transport: "streamable-http", URL: httpServer.URL, Credential: "env:KEY", Headers: map[string]string{"X-Key": "env:KEY"}}
	id, _ := Identity("mcp", filepath.Join(t.TempDir(), "mcp.json"))
	if _, err := ConnectMCP(t.Context(), id, c, func(string) string { return "" }); err == nil {
		t.Fatal("missing reference accepted")
	}
	conn, err := ConnectMCP(t.Context(), id, c, func(string) string { return secret })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	snapshot, _ := conn.Snapshot()
	catalog, _ := json.Marshal(snapshot.Export())
	if strings.Contains(string(raw)+string(catalog), secret) {
		t.Fatal("credential persisted")
	}
	conn.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpServer.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c.URL = redirect.URL
	if _, err := ConnectMCP(t.Context(), id, c, func(string) string { return secret }); err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), redirect.URL) {
		t.Fatal("unsafe redirect or diagnostic", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	c.Transport, c.Command, c.Args, c.URL, c.Credential, c.Headers = "stdio", "/bin/sh", []string{"-c", "sleep 10"}, "", "", nil
	started := time.Now()
	if _, err := ConnectMCP(ctx, id, c, nil); err == nil || time.Since(started) > time.Second*2 {
		t.Fatal("negotiation did not stop owned process", err)
	}
}

func TestMCPStdioLeaderExitClosesInheritedPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	identity, _ := Identity("mcp", filepath.Join(dir, "mcp.json"))
	config := MCPConfig{SchemaVersion: 1, Name: "exiting", Transport: "stdio", Command: "/bin/sh", Args: []string{"-c", `trap '' TERM; sleep 30 & printf '%s' "$!" > "$1"; exit 0`, "fixture", pidFile}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := ConnectMCP(ctx, identity, config, nil); err == nil || time.Since(start) > 2*time.Second {
		t.Fatal("inherited stdout kept connection alive", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 1 {
		t.Fatal(string(data), err)
	}
	// WSL's PID 1 may defer reaping an orphan. A zombie is stopped; a live
	// child holding stdout proves process-group cleanup failed.
	deadline := time.Now().Add(time.Second)
	for {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_, tail, ok := strings.Cut(string(stat), ") ")
		if ok && strings.HasPrefix(tail, "Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MCP child still alive", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMCPPreparationRejectsMissingDependencyAndChangedSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mcp.json")
	write := func(config MCPConfig) {
		t.Helper()
		b, _ := json.Marshal(config)
		if err := os.WriteFile(source, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(MCPConfig{SchemaVersion: 1, Name: "missing", Transport: "stdio", Command: filepath.Join(dir, "not-installed")})
	_, conn, err := PrepareMCP(t.Context(), source, nil)
	var known *protocol.Error
	if conn != nil || !errors.As(err, &known) || known.Data.Kind != "unavailable" {
		t.Fatal(conn, err)
	}
	fixture := resourceMCPFixture()
	fixture.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" {
				if err := os.WriteFile(source, []byte("unfinished"), 0600); err != nil {
					t.Error(err)
				}
			}
			return next(ctx, method, req)
		}
	})
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return fixture }, nil))
	defer httpServer.Close()
	write(MCPConfig{SchemaVersion: 1, Name: "changing", Transport: "streamable-http", URL: httpServer.URL})
	_, conn, err = PrepareMCP(t.Context(), source, nil)
	if conn != nil || !errors.As(err, &known) || known.Data.Kind != "resource_changed" {
		t.Fatal(conn, err)
	}
}
