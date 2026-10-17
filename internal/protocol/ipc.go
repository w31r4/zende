package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Handler interface {
	Handle(context.Context, Request) (any, error)
	Subscribe(context.Context, Request, func(Event) error) error
}

// Server holds its lock for the entire socket lifetime, including stale-socket
// cleanup. A competing starter can never unlink the live owner's socket.
type Server struct {
	listener net.Listener
	lock     *os.File
	handler  Handler
	wg       sync.WaitGroup
}

func Listen(path string, handler Handler) (*Server, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, E("already_running", "profile service is already running")
	}
	fail := func(err error) (*Server, error) {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fail(fmt.Errorf("socket path is occupied by a non-socket file"))
		}
		if err := os.Remove(path); err != nil {
			return fail(err)
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return fail(err)
	}
	return &Server{listener: listener, lock: lock, handler: handler}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	defer func() {
		s.listener.Close()
		s.wg.Wait()
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
	}()
	stop := context.AfterFunc(ctx, func() { s.listener.Close() })
	defer stop()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	encoder := json.NewEncoder(conn)
	for scanner.Scan() {
		var req Request
		err := StrictDecode(scanner.Bytes(), &req)
		if err == nil {
			err = req.Validate()
		}
		var result any
		if err == nil {
			result, err = s.handler.Handle(ctx, req)
		}
		response := Response{JSONRPC: "2.0", Version: Version, ID: req.ID}
		if err == nil {
			response.Result, err = json.Marshal(result)
		}
		if err != nil {
			response.Error = wireError(err, req.OperationID)
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if encoder.Encode(response) != nil {
			return
		}
		if req.Method == "events.watch" && err == nil {
			// A reader detects disconnects even when no events are being produced.
			watchCtx, cancel := context.WithCancel(ctx)
			go func() { var b [1]byte; conn.Read(b[:]); cancel() }()
			err = s.handler.Subscribe(watchCtx, req, func(event Event) error {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				return encoder.Encode(Notification{JSONRPC: "2.0", Method: "event", Params: event})
			})
			cancel()
			if err != nil && ctx.Err() == nil {
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				encoder.Encode(Response{JSONRPC: "2.0", Version: Version, ID: req.ID, Error: wireError(err, req.OperationID)})
			}
			return
		}
	}
}

func wireError(err error, op string) *Error {
	var e *Error
	if !errors.As(err, &e) {
		e = E("internal", "operation failed; inspect the local service state")
	}
	copy := *e
	copy.Data.OperationID = op
	return &copy
}

type Client struct{ Socket string }

func (c Client) connect(ctx context.Context) (net.Conn, func(), error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, E("unavailable", "service is offline; start the profile core and retry with the same operation_id")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	return conn, func() { stop(); conn.Close() }, nil
}

func (c Client) Call(ctx context.Context, req Request, result any) error {
	if err := req.Validate(); err != nil {
		return err
	}
	frame, err := json.Marshal(req)
	if err != nil || len(frame) >= MaxFrameBytes {
		return wireError(E("invalid_params", "request exceeds frame budget"), req.OperationID)
	}
	conn, close, err := c.connect(ctx)
	if err != nil {
		return wireError(err, req.OperationID)
	}
	defer close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return unknown(req.OperationID)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	if !scanner.Scan() {
		return unknown(req.OperationID)
	}
	var resp Response
	if err := StrictDecode(scanner.Bytes(), &resp); err != nil {
		if req.OperationID != "" && !ReadOnly(req.Method) {
			return unknown(req.OperationID)
		}
		return wireError(err, req.OperationID)
	}
	if (resp.JSONRPC != "2.0" || resp.ID != req.ID || resp.Version != Version) && req.OperationID != "" && !ReadOnly(req.Method) {
		return unknown(req.OperationID)
	}
	if err := checkResponse(req, resp); err != nil {
		if resp.Error == nil && req.OperationID != "" && !ReadOnly(req.Method) {
			return unknown(req.OperationID)
		}
		return wireError(err, req.OperationID)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		if req.OperationID != "" && !ReadOnly(req.Method) {
			return unknown(req.OperationID)
		}
		return err
	}
	return nil
}

func unknown(op string) *Error {
	e := E("transport_unknown", "connection ended before confirmation; inspect or retry using the same operation_id")
	e.Data.OperationID = op
	e.Data.Retryable = true
	return e
}

func checkResponse(req Request, resp Response) error {
	if resp.JSONRPC != "2.0" || resp.ID != req.ID || resp.Version != Version {
		return E("version_mismatch", "invalid or incompatible response envelope")
	}
	if resp.Error != nil {
		return resp.Error
	}
	if len(resp.Result) == 0 {
		return E("invalid_request", "response has no result")
	}
	return nil
}

// Watch emits only events, never commands. The caller persists the last consumed
// cursor and may reconnect from it without resubmitting the original task.
func (c Client) Watch(ctx context.Context, cursor Cursor, consume func(Event) error) error {
	return c.WatchReady(ctx, cursor, nil, consume)
}

// WatchReady reports the validated subscription acknowledgement, including an
// idle stream. It does not imply the requested history has finished replaying.
func (c Client) WatchReady(ctx context.Context, cursor Cursor, ready func(), consume func(Event) error) error {
	req, err := NewRequest("events.watch", "", cursor)
	if err != nil {
		return err
	}
	conn, close, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	if !scanner.Scan() {
		return io.ErrUnexpectedEOF
	}
	var ack Response
	if err := StrictDecode(scanner.Bytes(), &ack); err != nil {
		return err
	}
	if err := checkResponse(req, ack); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	last := cursor.After
	for scanner.Scan() {
		var frame struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return err
		}
		if frame.Method == "" {
			var resp Response
			if err := StrictDecode(scanner.Bytes(), &resp); err != nil {
				return err
			}
			return checkResponse(req, resp)
		}
		var notification Notification
		if err := StrictDecode(scanner.Bytes(), &notification); err != nil {
			return err
		}
		event := notification.Params
		if notification.JSONRPC != "2.0" || notification.Method != "event" || event.Version != Version || event.AgentID != cursor.AgentID {
			return E("invalid_request", "unexpected event envelope")
		}
		if event.Seq <= last {
			continue
		}
		if event.Seq != last+1 {
			return E("cursor_gap", "event sequence has a gap")
		}
		if err := consume(event); err != nil {
			return err
		}
		last = event.Seq
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scanner.Err() != nil {
		return scanner.Err()
	}
	return io.EOF
}

// Retry uses a bounded schedule. Mutations must have a durable operation ID.
// Only transport failures are retried; provider/tool effects are not replayed here.
func (c Client) Retry(ctx context.Context, req Request, result any, attempts int, base time.Duration) error {
	if attempts < 1 || attempts > 10 || base <= 0 || base > time.Minute {
		return E("invalid_params", "invalid retry bounds")
	}
	var err error
	var ambiguous *Error
	for n := 0; n < attempts; n++ {
		err = c.Call(ctx, req, result)
		var e *Error
		if err == nil || !errors.As(err, &e) || (e.Data.Kind != "unavailable" && e.Data.Kind != "transport_unknown") {
			return err
		}
		if e.Data.Kind == "transport_unknown" {
			ambiguous = e
		}
		if req.OperationID == "" && !ReadOnly(req.Method) {
			return err
		}
		if n == attempts-1 {
			break
		}
		timer := time.NewTimer(min(base*time.Duration(1<<n), 5*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			if ambiguous != nil {
				return ambiguous
			}
			return ctx.Err()
		case <-timer.C:
		}
		req.ID = NewID("rpc")
	}
	if ambiguous != nil {
		return ambiguous
	}
	return err
}

func ReadOnly(method string) bool {
	switch method {
	case "agent.select.preview":
		return true
	case "agent.copy.inspect", "agent.copy.drafts":
		return true
	case "config.read", "config.history", "config.runner":
		return true
	case "resource.watch.status":
		return true
	case "context.inspect", "context.read", "context.entry":
		return true
	case "plugin.events.list", "plugin.events.inspect", "plugin.events.read":
		return true
	case "agent.wait", "agent.result", "mailbox.list", "mailbox.inspect", "question.inspect", "question.list", "resource.list", "resource.status", "resource.read", "mcp.search", "mcp.inspect", "mcp.result", "plugin.catalog", "plugin.instances", "plugin.inspect", "plugin.progress", "plugin.result":
		return true
	case "core.status", "config.inspect", "agent.list", "agent.inspect", "agent.activity", "agent.history", "agent.recovery", "operation.inspect", "events.watch", "runner.status", "process.list", "process.inspect", "process.output", "process.wait", "process.control.inspect", "process.output.retention", "process.output.rotation.plan", "process.output.rotation.inspect", "attachment.inspect", "attachment.read", "attachment.list":
		return true
	}
	return false
}
