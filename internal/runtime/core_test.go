package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
)

func serveTest(t *testing.T) (*Core, protocol.Client, context.CancelFunc, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := New(s, resource.Effective{Config: resource.Defaults(dir)})
	socket := filepath.Join(dir, "core.sock")
	server, err := protocol.Listen(socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(cancel)
	return c, protocol.Client{Socket: socket}, cancel, done
}

func TestCoreSingleOwnerAndClientDetach(t *testing.T) {
	c, client, cancel, done := serveTest(t)
	if other, err := protocol.Listen(client.Socket, c); err == nil {
		_ = other
		t.Fatal("duplicate owner")
	}
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "persistent work"}
	req, _ := protocol.NewRequest("agent.create", "once", p)
	ctx, stop := context.WithTimeout(context.Background(), time.Second*5)
	defer stop()
	var accepted protocol.Accepted
	if err := client.Call(ctx, req, &accepted); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry := req
			retry.ID = protocol.NewID("rpc")
			var got protocol.Accepted
			if err := client.Call(ctx, retry, &got); err != nil || got != accepted {
				t.Errorf("%+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	inspect, _ := protocol.NewRequest("agent.inspect", "", protocol.Target{AgentID: accepted.AgentID})
	var snap protocol.Snapshot
	if err := client.Call(ctx, inspect, &snap); err != nil || len(snap.Tasks) != 1 {
		t.Fatal(snap, err)
	}
	status, _ := protocol.NewRequest("core.status", "", struct{}{})
	var result map[string]any
	if err := client.Call(ctx, status, &result); err != nil || result["instance_id"] != c.InstanceID {
		t.Fatal(result, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("core failed to stop")
	}
	server, err := protocol.Listen(client.Socket, c)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	ended, end := context.WithCancel(context.Background())
	end()
	if err := server.Serve(ended); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionReconnectAtSnapshotCursor(t *testing.T) {
	c, client, cancel, done := serveTest(t)
	defer func() { cancel(); <-done }()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "a"}
	req, _ := protocol.NewRequest("agent.create", "op", p)
	var a protocol.Accepted
	if err := client.Call(ctx, req, &a); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Store.Snapshot(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := c.Store.Append(ctx, a.AgentID, a.TaskID, "progress", struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []int64
	stop := errors.New("consumer stopped")
	watchCtx, end := context.WithTimeout(ctx, 5*time.Second)
	defer end()
	err = client.Watch(watchCtx, protocol.Cursor{AgentID: a.AgentID, After: snap.Cursor}, func(e protocol.Event) error {
		seqs = append(seqs, e.Seq)
		if len(seqs) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	last := seqs[len(seqs)-1]
	err = client.Watch(watchCtx, protocol.Cursor{AgentID: a.AgentID, After: last}, func(e protocol.Event) error {
		seqs = append(seqs, e.Seq)
		if len(seqs) == 5 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	for i, seq := range seqs {
		if seq != snap.Cursor+int64(i)+1 {
			t.Fatal(seqs)
		}
	}
	var got []protocol.Agent
	list, _ := protocol.NewRequest("agent.list", "", struct{}{})
	if err := client.Call(watchCtx, list, &got); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	err = client.Watch(watchCtx, protocol.Cursor{AgentID: a.AgentID, After: 1000}, func(protocol.Event) error { return nil })
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "cursor_gap" {
		t.Fatal(err)
	}
}

func TestLimitErrorPreservesExistingState(t *testing.T) {
	c, client, cancel, done := serveTest(t)
	defer func() { cancel(); <-done }()
	c.Config.Config.Limits.Agents = 1
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "existing work"}
	req, _ := protocol.NewRequest("agent.create", "first", p)
	var a protocol.Accepted
	if err := client.Call(context.Background(), req, &a); err != nil {
		t.Fatal(err)
	}
	req, _ = protocol.NewRequest("agent.create", "second", p)
	var result json.RawMessage
	err := client.Call(context.Background(), req, &result)
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "limit_exceeded" {
		t.Fatal(err)
	}
	snap, err := c.Store.Snapshot(context.Background(), a.AgentID)
	if err != nil || len(snap.Tasks) != 1 || snap.Tasks[0].Input != "existing work" {
		t.Fatal(snap, err)
	}
}

func TestRetryBoundedAndUnknownMethod(t *testing.T) {
	client := protocol.Client{Socket: filepath.Join(t.TempDir(), "absent.sock")}
	req, _ := protocol.NewRequest("core.status", "", struct{}{})
	start := time.Now()
	err := client.Retry(context.Background(), req, nil, 3, time.Millisecond)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal(err)
	}
	_, client, cancel, done := serveTest(t)
	defer func() { cancel(); <-done }()
	req, _ = protocol.NewRequest("does.not.exist", "", struct{}{})
	err = client.Call(context.Background(), req, nil)
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Data.Kind != "method_not_found" {
		t.Fatal(err)
	}
}
