package render

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestMain(m *testing.M) {
	if handled, code := process.RunInternalPTY(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type handoffClient struct {
	core, runner protocol.Client
	sync.Mutex
	resizeTargets []string
}

func (c *handoffClient) Retry(ctx context.Context, req protocol.Request, out any, attempts int, base time.Duration) error {
	client := c.core
	if strings.HasPrefix(req.Method, "process.") {
		client = c.runner
	}
	if req.Method == "process.resize" {
		var p process.ResizeParams
		json.Unmarshal(req.Params, &p)
		c.Lock()
		c.resizeTargets = append(c.resizeTargets, p.ID)
		c.Unlock()
	}
	return client.Retry(ctx, req, out, attempts, base)
}
func (c *handoffClient) Watch(ctx context.Context, cursor protocol.Cursor, f func(protocol.Event) error) error {
	return c.core.Watch(ctx, cursor, f)
}
func (c *handoffClient) WatchReady(ctx context.Context, cursor protocol.Cursor, ready func(), f func(protocol.Event) error) error {
	return c.core.WatchReady(ctx, cursor, ready, f)
}

func waitHandoff(t *testing.T, label string, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(label)
}

func TestRealTerminalHandoffTargetsOnePTYAndReplaysEvents(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	t.Setenv("TERM", "xterm-256color")
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manager, err := process.NewManager(filepath.Join(dir, "processes"), "/bin/sh", 4, 65536)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	adapter := heldAdapter{make(chan struct{}), make(chan struct{})}
	core.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	coreDone := make(chan error, 1)
	go func() { coreDone <- core.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-coreDone:
		case <-time.After(5 * time.Second):
			t.Error("core cleanup timeout")
		}
	}()
	startServer := func(name string, handler protocol.Handler) protocol.Client {
		path := filepath.Join(dir, name)
		server, err := protocol.Listen(path, handler)
		if err != nil {
			t.Fatal(err)
		}
		serverCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- server.Serve(serverCtx) }()
		t.Cleanup(func() {
			stop()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("server cleanup timeout")
			}
		})
		return protocol.Client{Socket: path}
	}
	client := &handoffClient{core: startServer("core.sock", core), runner: startServer("runner.sock", process.Service{Manager: manager})}
	req, _ := protocol.NewRequest("agent.create", "original", protocol.CreateAgent{Cwd: dir, Input: "work through terminal takeover"})
	var accepted protocol.Accepted
	if err := client.core.Call(ctx, req, &accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model not started")
	}
	var first process.State
	defer func() {
		for _, id := range []string{"pane-a", "pane-b", "pipe"} {
			_, _ = manager.Signal(context.Background(), "clean-"+id, process.SignalParams{ID: id, Signal: "KILL"})
			_, _ = manager.Wait(context.Background(), id, 5*time.Second)
		}
		if first.Terminal != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = exec.CommandContext(cleanup, "tmux", "-S", first.Terminal.Socket, "kill-server").Run()
		}
	}()
	for _, id := range []string{"pane-a", "pane-b", "pipe"} {
		state, err := manager.Start(ctx, process.Spec{ID: id, AgentID: accepted.AgentID, TaskID: accepted.TaskID, Cwd: dir, PTY: id != "pipe", Command: "printf ready; while IFS= read -r line; do printf 'received:%s\\n' \"$line\"; done"})
		if err != nil {
			t.Fatal(err)
		}
		if id == "pane-a" {
			first = state
		}
	}
	master, slave, capture := testTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	type observed struct {
		cursor                   int64
		panel, taskState         string
		resizePending, attaching bool
	}
	observations := make(chan observed, 1024)
	uiCtx, stopUI := context.WithCancel(ctx)
	uiDone := make(chan error, 1)
	go func() {
		uiDone <- Run(uiCtx, client, accepted.AgentID, slave, slave, tea.WithWindowSize(80, 24), tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(current tea.Model, msg tea.Msg) tea.Msg {
			m := current.(Shell)
			v := observed{cursor: m.cursor, resizePending: m.resizePending != nil, attaching: m.attaching}
			if m.panel != nil {
				v.panel = m.panel.id
			}
			for _, entry := range m.transcript.Entries {
				if entry.Kind == "user" && entry.TaskID == accepted.TaskID {
					v.taskState = entry.State
				}
			}
			select {
			case observations <- v:
			default:
			}
			return msg
		}))
	}()
	joined := false
	defer func() {
		stopUI()
		if !joined {
			select {
			case <-uiDone:
			case <-time.After(5 * time.Second):
				t.Error("UI cleanup timeout")
			}
		}
	}()
	waitTerminal(t, capture, "work through terminal takeover")
	send := func(text string) {
		t.Helper()
		if _, err := master.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	command := func(text string) { send("\x1b[200~" + text + "\x1b[201~\r") }
	command("/terminal pane-a")
	waitHandoff(t, "selected pane not ready", func() bool {
		select {
		case v := <-observations:
			return v.panel == "pane-a" && !v.resizePending
		default:
			return false
		}
	})
	resize := func(cols, rows uint16) {
		t.Helper()
		if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows}); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
			t.Fatal(err)
		}
	}
	resize(60, 20)
	waitHandoff(t, "selected PTY was not resized", func() bool {
		state, err := manager.Inspect(ctx, "pane-a")
		return err == nil && state.Terminal.Cols == 60 && state.Terminal.Rows == 20
	})
	waitHandoff(t, "resize did not settle in UI", func() bool {
		select {
		case v := <-observations:
			return v.panel == "pane-a" && !v.resizePending
		default:
			return false
		}
	})
	command("/attach")
	waitHandoff(t, "tmux client did not attach", func() bool {
		query, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		data, err := exec.CommandContext(query, "tmux", "-S", first.Terminal.Socket, "list-clients", "-F", "#{client_session}").Output()
		return err == nil && strings.TrimSpace(string(data)) == first.Terminal.Session
	})
	send("payload\r")
	waitHandoff(t, "raw input did not reach the original PTY", func() bool {
		out, err := manager.Read(ctx, "pane-a", "pty", 0, 65536)
		return err == nil && strings.Contains(string(out.Data), "received:payload")
	})
	resize(70, 22)
	waitHandoff(t, "handoff resize did not reach selected PTY", func() bool {
		state, err := manager.Inspect(ctx, "pane-a")
		return err == nil && state.Terminal.Cols == 70 && state.Terminal.Rows == 22
	})
	other, err := manager.Inspect(ctx, "pane-b")
	if err != nil || other.Terminal.Cols != 80 || other.Terminal.Rows != 24 {
		t.Fatal("resize broadcast", other, err)
	}
	close(adapter.release)
	var completed protocol.Snapshot
	waitHandoff(t, "model did not finish during takeover", func() bool {
		completed, err = s.Snapshot(ctx, accepted.AgentID)
		return err == nil && completed.Tasks[0].State == "completed"
	})
	send("\x02d")
	waitHandoff(t, "TUI did not replay the terminal event after handoff", func() bool {
		select {
		case v := <-observations:
			return !v.attaching && v.taskState == "completed" && v.cursor >= completed.Cursor
		default:
			return false
		}
	})
	send("\x03")
	select {
	case err := <-uiDone:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI detach timeout")
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || *before != *after {
		t.Fatal("terminal mode not restored", err)
	}
	state, err := manager.Inspect(ctx, "pane-a")
	if err != nil || state.Status != "running" || state.PID != first.PID {
		t.Fatal("handoff stopped/replaced original PTY", state, err)
	}
	client.Lock()
	defer client.Unlock()
	if len(client.resizeTargets) < 2 {
		t.Fatal("resize path not exercised")
	}
	for _, id := range client.resizeTargets {
		if id != "pane-a" {
			t.Fatal("another process received resize", id)
		}
	}
	if len(completed.Tasks) != 1 || completed.Tasks[0].ID != accepted.TaskID {
		t.Fatal("handoff duplicated task")
	}
}
