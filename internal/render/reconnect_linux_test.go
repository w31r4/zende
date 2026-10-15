package render

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

type observedClient struct {
	protocol.Client
	sync.Mutex
	cursors   []int64
	last      int64
	mutations int
}

func (c *observedClient) Retry(ctx context.Context, req protocol.Request, result any, attempts int, base time.Duration) error {
	c.Lock()
	if req.OperationID != "" {
		c.mutations++
	}
	c.Unlock()
	return c.Client.Retry(ctx, req, result, attempts, base)
}
func (c *observedClient) WatchReady(ctx context.Context, cursor protocol.Cursor, ready func(), consume func(protocol.Event) error) error {
	c.Lock()
	c.cursors = append(c.cursors, cursor.After)
	c.Unlock()
	return c.Client.WatchReady(ctx, cursor, ready, func(event protocol.Event) error {
		err := consume(event)
		c.Lock()
		c.last = event.Seq
		c.Unlock()
		return err
	})
}

func TestRealTerminalReconnectAfterOfflineTaskCompletion(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	adapter := heldAdapter{make(chan struct{}), make(chan struct{})}
	core.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	coreCtx, stopCore := context.WithCancel(context.Background())
	coreDone := make(chan error, 1)
	go func() { coreDone <- core.Run(coreCtx) }()
	defer func() {
		stopCore()
		if err := <-coreDone; err != nil {
			t.Error(err)
		}
	}()
	socket := filepath.Join(dir, "core.sock")
	startServer := func() func() {
		server, err := protocol.Listen(socket, core)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- server.Serve(ctx) }()
		return func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("server cleanup timeout")
			}
		}
	}
	stopServer := startServer()
	defer func() {
		if stopServer != nil {
			stopServer()
		}
	}()
	client := &observedClient{Client: protocol.Client{Socket: socket}}
	req, _ := protocol.NewRequest("agent.create", "original", protocol.CreateAgent{Cwd: dir, Input: "keep working"})
	var accepted protocol.Accepted
	if err := client.Call(coreCtx, req, &accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model not started")
	}
	master, slave, capture := testTerminal(t)
	uiCtx, stopUI := context.WithCancel(context.Background())
	uiDone := make(chan error, 1)
	observedFinal := make(chan struct{})
	var finalOnce sync.Once
	go func() {
		uiDone <- Run(uiCtx, client, accepted.AgentID, slave, slave, tea.WithWindowSize(80, 24), tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(current tea.Model, msg tea.Msg) tea.Msg {
			m := current.(Shell)
			for _, entry := range m.transcript.Entries {
				if entry.TaskID == accepted.TaskID && entry.Kind == "user" && entry.State == "completed" && m.input.Value() == "saved draft" {
					finalOnce.Do(func() { close(observedFinal) })
				}
			}
			return msg
		}))
	}()
	defer func() {
		stopUI()
		select {
		case <-uiDone:
		case <-time.After(5 * time.Second):
			t.Error("UI cleanup timeout")
		}
	}()
	waitTerminal(t, capture, "keep working")
	if _, err := master.Write([]byte("\x1b[200~saved draft\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	stopServer()
	stopServer = nil
	waitTerminal(t, capture, "offline")
	close(adapter.release)
	var snapshot protocol.Snapshot
	for deadline := time.Now().Add(5 * time.Second); ; {
		snapshot, err = s.Snapshot(coreCtx, accepted.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Tasks[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline task did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopServer = startServer()
	waitTerminal(t, capture, "finished while UI detached")
	select {
	case <-observedFinal:
	case <-time.After(5 * time.Second):
		t.Fatal("replayed terminal state or draft missing from UI model")
	}
	// Wait until the terminal event, not just the preceding model text, arrives.
	for deadline := time.Now().Add(5 * time.Second); ; {
		client.Lock()
		last := client.last
		client.Unlock()
		if last >= snapshot.Cursor {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal state not replayed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.Lock()
	defer client.Unlock()
	if client.mutations != 0 || len(client.cursors) < 2 || client.cursors[0] != 0 || client.cursors[len(client.cursors)-1] <= 0 {
		t.Fatal("reconnect replayed work or lost cursor", client.mutations, client.cursors)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != accepted.TaskID {
		t.Fatal("new task created", snapshot.Tasks)
	}
}
