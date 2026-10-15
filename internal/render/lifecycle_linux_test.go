package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

type terminalCapture struct {
	sync.Mutex
	data bytes.Buffer
}

func (c *terminalCapture) Write(p []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	return c.data.Write(p)
}
func (c *terminalCapture) contains(text string) bool {
	c.Lock()
	defer c.Unlock()
	return bytes.Contains(c.data.Bytes(), []byte(text))
}

func testTerminal(t *testing.T) (*os.File, *os.File, *terminalCapture) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "test-ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 24}); err != nil {
		t.Fatal(err)
	}
	capture := &terminalCapture{}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(capture, master); close(done) }()
	t.Cleanup(func() {
		_ = master.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("terminal reader did not stop")
		}
	})
	return master, slave, capture
}

func waitTerminal(t *testing.T, capture *terminalCapture, text string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if capture.contains(text) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("terminal did not emit %q", text)
}

type lifecycleClient struct {
	watchDone chan struct{}
	watching  chan struct{}
}

func (c *lifecycleClient) Retry(_ context.Context, req protocol.Request, target any, _ int, _ time.Duration) error {
	if req.Method != "agent.inspect" {
		return fmt.Errorf("unexpected mutation %s", req.Method)
	}
	*target.(*protocol.Snapshot) = protocol.Snapshot{Agent: protocol.Agent{ID: "agent", Cwd: "/project"}}
	return nil
}
func (c *lifecycleClient) Watch(ctx context.Context, _ protocol.Cursor, _ func(protocol.Event) error) error {
	close(c.watching)
	defer close(c.watchDone)
	<-ctx.Done()
	return ctx.Err()
}

func TestRealTerminalRestoresModesOnDetachCancellationAndPanic(t *testing.T) {
	for _, mode := range []string{"detach", "cancel", "panic"} {
		t.Run(mode, func(t *testing.T) {
			master, slave, capture := testTerminal(t)
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &lifecycleClient{watchDone: make(chan struct{}), watching: make(chan struct{})}
			options := []tea.ProgramOption{tea.WithWindowSize(80, 24), tea.WithColorProfile(colorprofile.ASCII)}
			if mode == "panic" {
				options = append(options, tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
					if _, ok := msg.(tea.KeyPressMsg); ok {
						panic("injected UI failure")
					}
					return msg
				}))
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, client, "agent", slave, slave, options...) }()
			waitTerminal(t, capture, "\x1b[?1049h")
			select {
			case <-client.watching:
			case <-time.After(5 * time.Second):
				t.Fatal("watch never started")
			}
			switch mode {
			case "cancel":
				cancel()
			case "panic":
				_, err = master.Write([]byte("x"))
			default:
				_, err = master.Write([]byte{3})
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if mode == "detach" && err != nil || mode == "cancel" && !errors.Is(err, tea.ErrProgramKilled) || mode == "panic" && !errors.Is(err, tea.ErrProgramPanic) {
					t.Fatal(mode, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("UI did not exit")
			}
			waitTerminal(t, capture, "\x1b[?1049l")
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("terminal modes not restored", before, after, err)
			}
			select {
			case <-client.watchDone:
			case <-time.After(time.Second):
				t.Fatal("subscription leaked")
			}
		})
	}
}

type heldAdapter struct{ entered, release chan struct{} }

func (a heldAdapter) Stream(ctx context.Context, _ model.Request, emit func(model.Delta) error) error {
	close(a.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.release:
	}
	for _, event := range fixture.Answer("final", "finished while UI detached") {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func TestRealTerminalDetachLeavesCoreTaskAlive(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	adapter := heldAdapter{make(chan struct{}), make(chan struct{})}
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	server, err := protocol.Listen(filepath.Join(dir, "core.sock"), c)
	if err != nil {
		t.Fatal(err)
	}
	coreCtx, stopCore := context.WithCancel(context.Background())
	serverDone, coreDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- server.Serve(coreCtx) }()
	go func() { coreDone <- c.Run(coreCtx) }()
	defer func() {
		stopCore()
		for _, done := range []chan error{serverDone, coreDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("core cleanup timed out")
			}
		}
	}()
	client := protocol.Client{Socket: filepath.Join(dir, "core.sock")}
	req, _ := protocol.NewRequest("agent.create", "initial", protocol.CreateAgent{Cwd: dir, Input: "keep working"})
	var accepted protocol.Accepted
	if err := client.Call(context.Background(), req, &accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model not started")
	}
	master, slave, capture := testTerminal(t)
	uiCtx, stopUI := context.WithCancel(context.Background())
	defer stopUI()
	done := make(chan error, 1)
	go func() {
		done <- Run(uiCtx, client, accepted.AgentID, slave, slave, tea.WithWindowSize(80, 24), tea.WithColorProfile(colorprofile.ASCII))
	}()
	waitTerminal(t, capture, "\x1b[?1049h")
	if _, err := master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach timed out")
	}
	snapshot, err := s.Snapshot(context.Background(), accepted.AgentID)
	if err != nil || snapshot.Tasks[0].State != "running" {
		t.Fatal("detach stopped background work", snapshot, err)
	}
	close(adapter.release)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		snapshot, err = s.Snapshot(context.Background(), accepted.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Tasks[0].State == "completed" {
			if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != accepted.TaskID || snapshot.Tasks[0].Result != "finished while UI detached" {
				t.Fatal(snapshot)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("core did not finish after detach", snapshot)
}
