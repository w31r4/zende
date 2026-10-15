package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
)

const RefreshTimeout = 30 * time.Second

// Connection owns one SDK session and its last complete catalog. It is a
// preparation layer: durable publication and model-visible commands belong to
// the resource host. Callers must Close it, including after failed publication.
type Connection struct {
	session                  *mcpsdk.ClientSession
	serverID, bindingVersion string
	life                     context.Context
	cancel                   context.CancelFunc
	refresh                  chan struct{}
	wake, updates            chan struct{}
	workers                  sync.WaitGroup
	closeOnce                sync.Once
	closeErr                 error
	mu                       sync.RWMutex
	snapshot                 *Snapshot
	lastError                *protocol.Error
	closed                   bool
}

type Status struct {
	State   string          `json:"state"`
	Version string          `json:"catalog_version,omitempty"`
	Tools   int             `json:"tools"`
	Error   *protocol.Error `json:"error,omitempty"`
}

// Connect bounds negotiation and initial listing. ctx governs preparation,
// not the returned connection's lifetime. Transport ownership transfers here;
// it must bound frames/events and Close must release its I/O/process resources.
// HTTP transports must apply ApplyCallHeaders to outgoing cloned requests.
// bindingVersion identifies nonsecret connection settings, never a credential.
func Connect(ctx context.Context, serverID, bindingVersion string, transport mcpsdk.Transport) (*Connection, error) {
	// Validate identity before starting a subprocess or network connection.
	if err := validateIdentity(serverID, bindingVersion); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, connectionError(ctx, ctx.Err())
	}
	if transport == nil {
		return nil, protocol.E("invalid_params", "MCP transport is required")
	}
	life, cancel := context.WithCancel(context.Background())
	c := &Connection{serverID: serverID, bindingVersion: bindingVersion, life: life, cancel: cancel,
		refresh: make(chan struct{}, 1), wake: make(chan struct{}, 1), updates: make(chan struct{}, 1)}
	ctx, stop := context.WithTimeout(ctx, RefreshTimeout)
	defer stop()
	stopPreparation := context.AfterFunc(ctx, cancel)
	defer stopPreparation()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "zende", Version: "1"}, &mcpsdk.ClientOptions{
		Capabilities:           &mcpsdk.ClientCapabilities{}, // no roots, sampling or elicitation
		MultiRoundTrip:         &mcpsdk.MultiRoundTripOptions{Disabled: true},
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) { signal(c.wake) },
	})
	client.AddSendingMiddleware(auditToolPages)
	client.AddSendingMiddleware(pinnedCallContext)
	client.AddSendingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "subscriptions/listen" {
				// The SDK uses a background context here. Its initial transport
				// write must still obey preparation cancellation, while a healthy
				// subscription outlives the initial Connect request.
				listenCtx, cancelListen := context.WithCancel(ctx)
				stopLife := context.AfterFunc(life, cancelListen)
				context.AfterFunc(listenCtx, func() { stopLife() })
				ctx = listenCtx
			}
			return next(ctx, method, req)
		}
	})
	captured := &captureTransport{Transport: transport}
	session, err := client.Connect(ctx, captured, nil)
	if err != nil {
		cancel()
		// Also covers a failure opening modern subscriptions after negotiation.
		// Return the original connection unchanged: wrapping it would hide SDK
		// internal session-update hooks used by the HTTP transport.
		if captured.connection != nil {
			_ = captured.connection.Close()
		}
		return nil, connectionError(ctx, err)
	}
	c.session = session
	result := session.InitializeResult()
	if result == nil || result.Capabilities == nil || result.Capabilities.Tools == nil {
		cancel()
		_ = session.Close()
		return nil, protocol.E("invalid_resource", "MCP server does not advertise tools")
	}
	if err := c.Refresh(ctx); err != nil {
		cancel()
		_ = session.Close()
		if ctx.Err() != nil {
			return nil, connectionError(ctx, ctx.Err())
		}
		return nil, err
	}
	stopPreparation()
	if ctx.Err() != nil || life.Err() != nil {
		cancel()
		_ = session.Close()
		return nil, connectionError(ctx, ctx.Err())
	}
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		_ = session.Wait()
		cancel()
		signal(c.updates)
	}()
	go func() {
		defer c.workers.Done()
		for {
			select {
			case <-life.Done():
				return
			case <-c.wake:
				_ = c.Refresh(life)
			}
		}
	}()
	return c, nil
}

type captureTransport struct {
	mcpsdk.Transport
	connection mcpsdk.Connection
}

func (t *captureTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	t.connection = connection
	return connection, err
}

type pageAuditKey struct{}
type pageAudit struct{ count int }

// The SDK filters malformed tools after middleware and caches list pages. We
// reject a lossy page instead of publishing a silently shortened catalog, and
// turn off page caching so explicit refresh really reads the server each time.
func auditToolPages(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		result, err := next(ctx, method, req)
		if method == "tools/list" && err == nil {
			if page, ok := result.(*mcpsdk.ListToolsResult); ok && page != nil {
				if audit, ok := ctx.Value(pageAuditKey{}).(*pageAudit); ok && page.Tools != nil {
					audit.count = len(page.Tools)
				}
				page.TTLMs = 0
			}
		}
		return result, err
	}
}

func (c *Connection) list(ctx context.Context, cursor string) (*mcpsdk.ListToolsResult, error) {
	audit := &pageAudit{count: -1}
	page, err := c.session.ListTools(context.WithValue(ctx, pageAuditKey{}, audit), &mcpsdk.ListToolsParams{Cursor: cursor})
	if err != nil {
		return nil, connectionError(ctx, err)
	}
	if page == nil || audit.count < 0 || audit.count != len(page.Tools) {
		return nil, invalidCatalog()
	}
	return page, nil
}

// Refresh serializes with other refreshes, not searches. Failure preserves the
// old snapshot; close/disconnect/cancellation fences publication.
func (c *Connection) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, RefreshTimeout)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	select {
	case <-ctx.Done():
		return connectionError(ctx, ctx.Err())
	case c.refresh <- struct{}{}:
	}
	defer func() { <-c.refresh }()
	snapshot, err := prepare(ctx, c.serverID, c.bindingVersion, c.list)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.life.Err() != nil {
		return protocol.E("unavailable", "MCP connection is closed; reconnect before discovery")
	}
	if err == nil && ctx.Err() != nil {
		err = connectionError(ctx, ctx.Err())
	}
	if err != nil {
		var diagnostic *protocol.Error
		if !errors.As(err, &diagnostic) {
			diagnostic = connectionError(ctx, err)
		}
		saved := *diagnostic
		c.lastError = &saved
		signal(c.updates)
		return diagnostic
	}
	changed := c.snapshot == nil || c.snapshot.version != snapshot.version || c.lastError != nil
	c.snapshot, c.lastError = snapshot, nil
	if changed {
		signal(c.updates)
	}
	return nil
}

func (c *Connection) Snapshot() (*Snapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || c.life.Err() != nil || c.snapshot == nil {
		return nil, protocol.E("unavailable", "MCP connection is unavailable; reconnect before discovery")
	}
	return c.snapshot, nil
}

func (c *Connection) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := Status{State: "ready"}
	if c.snapshot != nil {
		s.Version, s.Tools = c.snapshot.Version(), c.snapshot.Len()
	}
	if c.lastError != nil {
		diagnostic := *c.lastError
		s.Error = &diagnostic
	}
	if c.life.Err() != nil {
		s.State = "unavailable"
	}
	if c.closed {
		s.State = "closed"
	}
	return s
}

// Updates coalesces wakeups; consumers query Status/Snapshot for current state.
// It is not a durable event log and is not closed. Done signals disconnection.
func (c *Connection) Updates() <-chan struct{} { return c.updates }
func (c *Connection) Done() <-chan struct{}    { return c.life.Done() }

func (c *Connection) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.cancel()
		if c.session.Close() != nil {
			c.closeErr = protocol.E("unavailable", "MCP transport did not close cleanly")
		}
		c.workers.Wait()
		signal(c.updates)
	})
	return c.closeErr
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Raw transport/server diagnostics can contain credentials or arbitrary text.
func connectionError(ctx context.Context, err error) *protocol.Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return protocol.E("timeout", "MCP preparation timed out")
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return protocol.E("cancelled", "MCP preparation was cancelled")
	}
	return protocol.E("unavailable", "MCP connection or tool listing failed")
}
