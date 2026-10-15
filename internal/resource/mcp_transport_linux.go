package resource

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
)

// MCPConnection owns transport resources as well as the negotiated session.
type MCPConnection struct {
	*mcp.Connection
	cleanup   func()
	once      sync.Once
	mu        sync.Mutex
	leases    int
	retired   bool
	closed    bool
	closeDone chan struct{}
}

func (c *MCPConnection) Close() error {
	var err error
	c.once.Do(func() {
		defer close(c.closeDone)
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		err = c.Connection.Close()
		c.cleanup()
	})
	return err
}

// Closed waits for transport cleanup, unlike Done which signals disconnection
// before HTTP session deletion or stdio process reaping has necessarily ended.
func (c *MCPConnection) Closed() <-chan struct{} { return c.closeDone }

// LeaseCall pins a validated call to this connection. Release must run even if
// journaling fails before dispatch. A retired connection accepts no new calls;
// existing leases drain normally. Close, used for Core shutdown, cancels all.
func (c *MCPConnection) LeaseCall(ctx context.Context, ref string, arguments []byte) (*mcp.Call, func(), error) {
	c.mu.Lock()
	if c.retired || c.closed {
		c.mu.Unlock()
		return nil, nil, protocol.E("stale_reference", "MCP connection was replaced; search again")
	}
	c.leases++
	c.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			c.leases--
			closeNow := c.retired && c.leases == 0
			c.mu.Unlock()
			if closeNow {
				_ = c.Close()
			}
		})
	}
	call, err := c.Connection.PrepareCall(ctx, ref, arguments)
	if err != nil {
		release()
		return nil, nil, err
	}
	return call, release, nil
}

// Retire is graceful hot replacement. It never cancels a leased invocation.
func (c *MCPConnection) Retire() {
	c.mu.Lock()
	c.retired = true
	closeNow := c.leases == 0
	c.mu.Unlock()
	if closeNow {
		_ = c.Close()
	}
}

// ConnectMCP resolves references only at the connection boundary. Config must
// come from ParseMCPConfig; preparation never persists resolved values.
func ConnectMCP(ctx context.Context, identity protocol.ResourceInfo, config MCPConfig, env func(string) string) (*MCPConnection, error) {
	if env == nil {
		env = os.Getenv
	}
	resolve := func(ref string) (string, error) {
		value, err := ResolveCredential(ref, env)
		if err != nil || len(value) > 65536 || strings.ContainsRune(value, 0) {
			return "", protocol.E("unavailable", "MCP credential reference is unavailable or invalid")
		}
		return value, nil
	}
	var transport mcpsdk.Transport
	cleanup := func() {}
	switch config.Transport {
	case "stdio":
		cmd := exec.Command(config.Command, config.Args...)
		cmd.Dir = config.Cwd
		if cmd.Dir == "" {
			cmd.Dir = filepath.Dir(identity.Source)
		}
		cmd.Env = os.Environ()
		for key, ref := range config.Env {
			value, err := resolve(ref)
			if err != nil {
				return nil, err
			}
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		// nil stderr goes to /dev/null without exec's copy goroutine, so an
		// escaped descendant cannot hold Wait open through an inherited pipe.
		cmd.Stderr = nil
		cmd.WaitDelay = 300 * time.Millisecond
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		transport = &mcpCommandTransport{cmd: cmd}
	case "streamable-http":
		headers := make(http.Header)
		for key, ref := range config.Headers {
			value, err := resolve(ref)
			if err != nil {
				return nil, err
			}
			if strings.IndexFunc(value, func(r rune) bool { return r < 32 && r != '\t' || r == 127 }) >= 0 {
				return nil, protocol.E("unavailable", "MCP header reference contains invalid bytes")
			}
			headers.Set(key, value)
		}
		if config.Credential != "" {
			value, err := resolve(config.Credential)
			if err != nil {
				return nil, err
			}
			if strings.IndexFunc(value, func(r rune) bool { return r <= 32 || r == 127 }) >= 0 {
				return nil, protocol.E("unavailable", "MCP credential reference contains invalid bytes")
			}
			headers.Set("Authorization", "Bearer "+value)
		}
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.ResponseHeaderTimeout = 30 * time.Second
		client := &http.Client{Transport: &mcpHeaders{base: base, headers: headers}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return protocol.E("unavailable", "MCP redirects are not followed")
		}}
		transport = &mcpsdk.StreamableClientTransport{Endpoint: config.URL, HTTPClient: client, MaxRetries: -1, MaxEventSize: 16 << 20}
		cleanup = base.CloseIdleConnections
	default:
		return nil, protocol.E("invalid_resource", "unsupported MCP transport")
	}
	conn, err := mcp.Connect(ctx, identity.ID, config.BindingVersion(), transport)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &MCPConnection{Connection: conn, cleanup: cleanup, closeDone: make(chan struct{})}, nil
}

type mcpHeaders struct {
	base    *http.Transport
	headers http.Header
}

func (t *mcpHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	for key, values := range t.headers {
		copy.Header[key] = append([]string(nil), values...)
	}
	mcp.ApplyCallHeaders(copy)
	return t.base.RoundTrip(copy)
}

type mcpCommandTransport struct{ cmd *exec.Cmd }

func (t *mcpCommandTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err = t.cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	wire, err := (&mcpsdk.IOTransport{Reader: stdout, Writer: stdin, MaxLineLength: 16 << 20}).Connect(ctx)
	owned := &mcpCommandConnection{Connection: wire, cmd: t.cmd, stop: make(chan struct{})}
	if err != nil {
		_ = owned.Close()
		return nil, err
	}
	go owned.monitor()
	return owned, nil
}

type mcpCommandConnection struct {
	mcpsdk.Connection
	cmd  *exec.Cmd
	once sync.Once
	stop chan struct{}
}

func (c *mcpCommandConnection) exited() bool {
	var info unix.Siginfo
	// WNOWAIT keeps the leader's PID reserved until all group signals are sent.
	err := unix.Waitid(unix.P_PID, c.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	return err == nil && info.Signo != 0
}

func (c *mcpCommandConnection) monitor() {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
			if c.exited() {
				_ = c.Close()
				return
			}
		}
	}
}

func (c *mcpCommandConnection) Close() error {
	c.once.Do(func() {
		close(c.stop)
		if c.Connection != nil {
			_ = c.Connection.Close()
		}
		pid := c.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		deadline := time.Now().Add(300 * time.Millisecond)
		for !c.exited() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		// The unreaped group leader prevents PID reuse, including when its
		// children hold stdout open after it exits. Reap only after the last kill.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = c.cmd.Wait()
	})
	return nil
}
