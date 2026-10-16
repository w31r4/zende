package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func TestPluginEventProcessHelper(t *testing.T) {
	if os.Getenv("ZENDE_PLUGIN_EVENT_HELPER") != "1" {
		return
	}
	mode := os.Getenv("ZENDE_PLUGIN_EVENT_MODE")
	var output sync.Mutex
	send := func(value any) {
		output.Lock()
		defer output.Unlock()
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(3)
		}
	}
	respond := func(id string, value any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": value}) }
	type state struct {
		params EventOpen
		cursor int64
		closed bool
	}
	var mu sync.Mutex
	subscriptions := map[string]*state{}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	for scanner.Scan() {
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(4)
		}
		f, err := os.OpenFile("audit", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(5)
		}
		err = json.NewEncoder(f).Encode(req)
		f.Close()
		if err != nil {
			os.Exit(5)
		}
		switch req.Method {
		case "plugin.initialize":
			var p InitializeParams
			_ = json.Unmarshal(req.Params, &p)
			respond(req.ID, InitializeResult{ProtocolVersion: 1, Plugin: p.Plugin, Capabilities: p.Capabilities})
		case "plugin.describe":
			respond(req.ID, catalogReply{Events: eventCapabilities()})
		case "plugin.shutdown":
			respond(req.ID, nil)
			os.Exit(0)
		case "plugin.events.open":
			var p EventOpen
			if json.Unmarshal(req.Params, &p) != nil || p.Scope.AgentID == "" || !filepath.IsAbs(p.Scope.Cwd) || !strings.HasPrefix(p.DefinitionRef, "plugin_event_") {
				os.Exit(6)
			}
			mu.Lock()
			subscriptions[p.SubscriptionID] = &state{params: p, cursor: p.After}
			mu.Unlock()
			ack := EventAck{p.SubscriptionID, p.After, "open"}
			if mode == "bad-open" {
				ack.SubscriptionID = "foreign"
			}
			respond(req.ID, ack)
		case "plugin.events.read":
			var p EventRead
			_ = json.Unmarshal(req.Params, &p)
			go func() {
				if mode == "read-delay" {
					time.Sleep(150 * time.Millisecond)
				}
				if mode == "read-error" {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "secret-event-gap"}})
					return
				}
				mu.Lock()
				s := subscriptions[p.SubscriptionID]
				if s == nil || s.closed || s.params.Direction != "plugin" || p.After > s.cursor {
					os.Exit(7)
				}
				page := PluginEventPage{SubscriptionID: p.SubscriptionID, After: p.After, Next: p.After, Events: []PluginEvent{}}
				for n := p.After + 1; n <= 3 && len(page.Events) < p.Limit; n++ {
					value, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("event-%d", n)})
					page.Events = append(page.Events, PluginEvent{n, "changed", value})
					page.Next = n
				}
				s.cursor = max(s.cursor, page.Next)
				switch mode {
				case "read-gap":
					page.Next++
				case "read-foreign":
					page.SubscriptionID = "foreign"
				case "read-order":
					page.Events[0].Sequence++
				case "read-schema":
					page.Events[0].Value = json.RawMessage(`{"text":42}`)
				case "read-duplicate-key":
					page.Events[0].Value = json.RawMessage(`{"text":"a","text":"b"}`)
				case "read-oversize":
					page.Events[0].Value, _ = json.Marshal(map[string]string{"text": strings.Repeat("x", MaxEventBytes)})
				case "read-closed":
					page.Closed, s.closed = true, true
				}
				mu.Unlock()
				if mode == "read-missing" {
					respond(req.ID, map[string]any{"subscription_id": p.SubscriptionID, "after": p.After, "next": p.After, "events": []PluginEvent{}})
					return
				}
				respond(req.ID, page)
			}()
		case "plugin.events.deliver":
			var p CoreEventBatch
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			s := subscriptions[p.SubscriptionID]
			if s == nil || s.closed || s.params.Direction != "core" || s.cursor != p.After {
				os.Exit(8)
			}
			s.cursor = p.Through
			mu.Unlock()
			ack := EventAck{p.SubscriptionID, p.Through, "open"}
			if mode == "bad-deliver" {
				ack.Cursor++
			}
			respond(req.ID, ack)
		case "plugin.events.close":
			var p EventClose
			_ = json.Unmarshal(req.Params, &p)
			go func() {
				if mode == "close-never" {
					return
				}
				if mode == "close-delay" {
					deadline := time.Now().Add(4 * time.Second)
					for {
						if _, err := os.Stat("release-close"); err == nil {
							break
						}
						if time.Now().After(deadline) {
							os.Exit(11)
						}
						time.Sleep(time.Millisecond)
					}
				}
				mu.Lock()
				s := subscriptions[p.SubscriptionID]
				if s == nil || s.cursor != p.Cursor {
					os.Exit(9)
				}
				s.closed = true
				mu.Unlock()
				ack := EventAck{p.SubscriptionID, p.Cursor, "closed"}
				if mode == "bad-close" {
					ack.State = "open"
				}
				respond(req.ID, ack)
				if mode == "close-exit" {
					os.Exit(0)
				}
			}()
		default:
			os.Exit(10)
		}
	}
	os.Exit(0)
}

func eventFixture(t *testing.T, mode string) (*Instance, *Catalog, string) {
	t.Helper()
	t.Setenv("ZENDE_PLUGIN_EVENT_HELPER", "1")
	t.Setenv("ZENDE_PLUGIN_EVENT_MODE", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := Manifest{SchemaVersion: 1, ID: "events", Version: "1", ProtocolVersions: []int{1}, Command: binary, Args: []string{"-test.run=^TestPluginEventProcessHelper$"}, Capabilities: []string{"events"}}
	i, err := Start(t.Context(), filepath.Join(dir, "plugin.json"), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = i.Close() })
	c, err := i.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i, c, dir
}

func subscribeEvents(t *testing.T, i *Instance, c *Catalog, direction, id string) *EventSubscription {
	t.Helper()
	d := c.Events()[slices.IndexFunc(c.Events(), func(d EventDefinition) bool { return d.Direction == direction })]
	p := EventSubscribe{OperationID: "op-" + id, SubscriptionID: id, DefinitionRef: d.Ref, Scope: Scope{AgentID: "agent", TaskID: "task"}}
	if direction == "core" {
		p.After, p.Kinds = 4, []string{"task.completed"}
	}
	s, err := i.OpenEvents(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPluginEventsBothDirectionsAndConfirmedClose(t *testing.T) {
	i, c, dir := eventFixture(t, "")
	s := subscribeEvents(t, i, c, "plugin", "source")
	page, err := s.Read(t.Context(), 0, 2, 0)
	if err != nil || page.Next != 2 || len(page.Events) != 2 || page.Closed {
		t.Fatal(page, err)
	}
	again, err := s.Read(t.Context(), 0, 2, 0)
	if err != nil || !reflect.DeepEqual(again, page) {
		t.Fatal("read consumed unacknowledged events", again, err)
	}
	last, err := s.Read(t.Context(), 2, 2, 0)
	if err != nil || last.Next != 3 || len(last.Events) != 1 {
		t.Fatal(last, err)
	}
	info := s.Info()
	if info.InstanceID != i.Info().InstanceID || info.DefinitionRef == "" || info.CatalogVersion != c.Version() || info.Cursor != 3 || info.State != "open" {
		t.Fatal(info)
	}
	listener := subscribeEvents(t, i, c, "core", "listener")
	event := protocol.Event{Version: protocol.Version, AgentID: "agent", TaskID: "task", Seq: 6, Kind: "task.completed", Payload: json.RawMessage(`{"result":"done"}`)}
	batch := CoreEventBatch{SubscriptionID: "listener", After: 4, Through: 7, Events: []protocol.Event{event}}
	if err := listener.Deliver(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if listener.Info().Cursor != 7 {
		t.Fatal(listener.Info())
	}
	for _, sub := range []*EventSubscription{s, listener} {
		if err := sub.Close(t.Context(), "close-"+sub.Info().SubscriptionID); err != nil {
			t.Fatal(err)
		}
		if sub.Info().State != "closed" {
			t.Fatal(sub.Info())
		}
	}
	before, _ := os.ReadFile(filepath.Join(dir, "audit"))
	if err := s.Close(t.Context(), "close-again"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(t.Context(), 3, 1, 0); err == nil {
		t.Fatal("read after close")
	}
	if err := listener.Deliver(t.Context(), batch); err == nil {
		t.Fatal("delivery after close")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "audit"))
	if !bytes.Equal(before, after) || i.Info().State != "ready" {
		t.Fatal("closed subscription dispatched or stopped instance")
	}
	if !bytes.Contains(after, []byte(`"seq":6`)) || !bytes.Contains(after, []byte(`"task_id":"task"`)) {
		t.Fatal("Core identity lost", string(after))
	}
}

func TestPluginEventsRejectMalformedReplies(t *testing.T) {
	for _, mode := range []string{"bad-open", "read-gap", "read-foreign", "read-order", "read-schema", "read-duplicate-key", "read-oversize", "read-missing", "bad-deliver", "bad-close"} {
		t.Run(mode, func(t *testing.T) {
			i, c, _ := eventFixture(t, mode)
			var err error
			if mode == "bad-open" {
				_, err = i.OpenEvents(t.Context(), EventSubscribe{OperationID: "op", SubscriptionID: "sub", DefinitionRef: c.Events()[1].Ref, Scope: Scope{AgentID: "agent"}})
			} else if mode == "bad-deliver" {
				s := subscribeEvents(t, i, c, "core", "sub")
				err = s.Deliver(t.Context(), CoreEventBatch{SubscriptionID: "sub", After: 4, Through: 5, Events: []protocol.Event{}})
			} else {
				s := subscribeEvents(t, i, c, "plugin", "sub")
				if mode == "bad-close" {
					err = s.Close(t.Context(), "close")
				} else {
					_, err = s.Read(t.Context(), 0, 2, 0)
				}
			}
			kind := "plugin_protocol"
			if mode == "read-duplicate-key" {
				kind = "transport_unknown" // Envelope reader rejects it before routing.
			}
			requireFailure(t, err, kind)
			if i.Info().State == "ready" {
				t.Fatal("malformed event response left instance ready")
			}
		})
	}
}

func TestPluginEventsPreflightDoesNotDispatch(t *testing.T) {
	i, c, dir := eventFixture(t, "")
	s := subscribeEvents(t, i, c, "plugin", "source")
	listener := subscribeEvents(t, i, c, "core", "listener")
	before, _ := os.ReadFile(filepath.Join(dir, "audit"))
	for _, args := range [][3]int64{{-1, 1, 0}, {1, 1, 0}, {0, 0, 0}, {0, 65, 0}, {0, 1, 1001}, {0, 1, -1}} {
		if _, err := s.Read(t.Context(), args[0], int(args[1]), int(args[2])); err == nil {
			t.Fatal(args)
		}
	}
	for _, alter := range []func(*CoreEventBatch){
		func(b *CoreEventBatch) { b.SubscriptionID = "foreign" },
		func(b *CoreEventBatch) { b.After = 3 },
		func(b *CoreEventBatch) { b.Events[0].AgentID = "other" },
		func(b *CoreEventBatch) { b.Events[0].Version++ },
		func(b *CoreEventBatch) { b.Events[0].Kind = "task.failed" },
		func(b *CoreEventBatch) { b.Events[0].Seq = 4 },
		func(b *CoreEventBatch) { b.Events[0].Payload = json.RawMessage(`{"x":1,"x":2}`) },
		func(b *CoreEventBatch) {
			b.Events[0].Payload, _ = json.Marshal(strings.Repeat("x", MaxEventBatchBytes))
		},
	} {
		b := CoreEventBatch{SubscriptionID: "listener", After: 4, Through: 5, Events: []protocol.Event{{Version: protocol.Version, AgentID: "agent", Seq: 5, Kind: "task.completed", Payload: json.RawMessage(`{}`)}}}
		alter(&b)
		if err := listener.Deliver(t.Context(), b); err == nil {
			t.Fatal(b)
		}
	}
	p := EventSubscribe{OperationID: "op", SubscriptionID: "new", DefinitionRef: c.Events()[1].Ref, Scope: Scope{AgentID: "agent"}}
	for _, alter := range []func(*EventSubscribe){
		func(p *EventSubscribe) { p.SubscriptionID = "source" },
		func(p *EventSubscribe) { p.Scope.AgentID = "" },
		func(p *EventSubscribe) { p.DefinitionRef = "foreign" },
		func(p *EventSubscribe) { p.After = 1 },
		func(p *EventSubscribe) { p.Kinds = []string{"task.completed"} },
	} {
		x := p
		alter(&x)
		if _, err := i.OpenEvents(t.Context(), x); err == nil {
			t.Fatal(x)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "audit"))
	if !bytes.Equal(before, after) || i.Info().State != "ready" {
		t.Fatal("invalid input reached plugin")
	}
}

func TestPluginEventsCloseWaitsAndDoesNotBlockOtherSubscription(t *testing.T) {
	i, c, dir := eventFixture(t, "close-delay")
	s := subscribeEvents(t, i, c, "plugin", "closing")
	other := subscribeEvents(t, i, c, "plugin", "other")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Close(ctx, "close") }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, _ := os.ReadFile(filepath.Join(dir, "audit"))
		if bytes.Contains(raw, []byte("plugin.events.close")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close not sent")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if s.Info().State != "closing" {
		t.Fatal("closure claimed before acknowledgement", s.Info())
	}
	select {
	case err := <-done:
		t.Fatal("close did not wait for confirmation", err)
	default:
	}
	if page, err := other.Read(t.Context(), 0, 1, 0); err != nil || page.Next != 1 {
		t.Fatal(page, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release-close"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || s.Info().State != "closed" || i.Info().State != "ready" {
		t.Fatal(err, s.Info(), i.Info())
	}
}

func TestPluginEventsCancelledReadDrainsAndFailureIsBounded(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		i, c, dir := eventFixture(t, "read-delay")
		s := subscribeEvents(t, i, c, "plugin", "source")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := s.Read(ctx, 0, 1, 0); done <- err }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			raw, _ := os.ReadFile(filepath.Join(dir, "audit"))
			if bytes.Contains(raw, []byte("plugin.events.read")) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("read not sent")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil || s.Info().Cursor != 1 || i.Info().State != "ready" {
			t.Fatal(err, s.Info(), i.Info())
		}
		if err := s.Close(t.Context(), "close"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		i, c, _ := eventFixture(t, "close-never")
		s := subscribeEvents(t, i, c, "plugin", "source")
		other, otherCatalog, _ := eventFixture(t, "")
		unrelated := subscribeEvents(t, other, otherCatalog, "plugin", "unrelated")
		start := time.Now()
		err := s.Close(t.Context(), "close")
		requireFailure(t, err, "transport_unknown")
		if time.Since(start) > EventRequestTimeout+2*time.Second || s.Info().State != "unknown" || i.Info().State == "ready" {
			t.Fatal(s.Info(), i.Info())
		}
		if page, err := unrelated.Read(t.Context(), 0, 1, 0); err != nil || page.Next != 1 || other.Info().State != "ready" {
			t.Fatal("failed subscription affected another instance", page, err)
		}
	})
	for _, mode := range []string{"read-error", "read-closed", "close-exit"} {
		t.Run(mode, func(t *testing.T) {
			i, c, _ := eventFixture(t, mode)
			s := subscribeEvents(t, i, c, "plugin", "source")
			if mode == "close-exit" {
				if err := s.Close(t.Context(), "close"); err != nil {
					t.Fatal(err)
				}
				return
			}
			p, err := s.Read(t.Context(), 0, 2, 0)
			if mode == "read-error" {
				requireFailure(t, err, "event_unavailable")
				if strings.Contains(err.Error(), "secret") || s.Info().Cursor != 0 {
					t.Fatal(err, s.Info())
				}
			} else if err != nil || !p.Closed || s.Info().State != "closed" {
				t.Fatal(p, err, s.Info())
			}
		})
	}
}

func TestPluginEventsLimitsAndCancelledEntry(t *testing.T) {
	i, c, dir := eventFixture(t, "")
	var subs []*EventSubscription
	for n := range MaxEventSubscriptions {
		subs = append(subs, subscribeEvents(t, i, c, "plugin", fmt.Sprint("sub-", n)))
	}
	p := EventSubscribe{OperationID: "extra", SubscriptionID: "extra", DefinitionRef: c.Events()[1].Ref, Scope: Scope{AgentID: "agent"}}
	if _, err := i.OpenEvents(t.Context(), p); err == nil {
		t.Fatal("subscription limit ignored")
	}
	if err := subs[0].Close(t.Context(), "close"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before, _ := os.ReadFile(filepath.Join(dir, "audit"))
	if _, err := i.OpenEvents(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "audit"))
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled open dispatched")
	}
	s, err := i.OpenEvents(t.Context(), p)
	if err != nil || s.Info().State != "open" {
		t.Fatal(s, err)
	}
}

func TestPluginEventAcknowledgementRequiresEveryField(t *testing.T) {
	if !eventAck([]byte(`{"subscription_id":"sub","cursor":0,"state":"open"}`), "sub", 0, "open") {
		t.Fatal("explicit zero cursor rejected")
	}
	for _, raw := range []string{
		`{"subscription_id":"sub","state":"open"}`,
		`{"subscription_id":"sub","cursor":null,"state":"open"}`,
		`{"subscription_id":"sub","cursor":0}`,
		`{"cursor":0,"state":"open"}`,
		`{"subscription_id":"sub","cursor":"0","state":"open"}`,
		`{"subscription_id":"sub","cursor":0,"state":"open","extra":true}`,
		`{"subscription_id":"sub","cursor":1,"cursor":0,"state":"open"}`,
		`{"subscription_id":"sub","cursor":0,"state":"closed"}`,
		`null`,
	} {
		if eventAck([]byte(raw), "sub", 0, "open") {
			t.Fatal("invalid acknowledgement accepted", raw)
		}
	}
}

func TestPluginEventsInstanceExitLeavesUnclosedSubscriptionUnknown(t *testing.T) {
	i, c, _ := eventFixture(t, "")
	open := subscribeEvents(t, i, c, "plugin", "open")
	closed := subscribeEvents(t, i, c, "plugin", "closed")
	if err := closed.Close(t.Context(), "close"); err != nil {
		t.Fatal(err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	if open.Info().State != "unknown" || closed.Info().State != "closed" {
		t.Fatal("transport closure replaced subscription evidence", open.Info(), closed.Info())
	}
	if err := open.Close(t.Context(), "late-close"); err == nil {
		t.Fatal("dead transport confirmed subscription closure")
	}
}

func TestPluginEventsSerializeReadBeforeClose(t *testing.T) {
	i, c, dir := eventFixture(t, "read-delay")
	s := subscribeEvents(t, i, c, "plugin", "source")
	read := make(chan error, 1)
	go func() {
		page, err := s.Read(t.Context(), 0, 1, 0)
		if err == nil && page.Next != 1 {
			err = fmt.Errorf("read cursor = %d", page.Next)
		}
		read <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, _ := os.ReadFile(filepath.Join(dir, "audit"))
		if bytes.Contains(raw, []byte("plugin.events.read")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read not dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	// A cancelled waiter cannot interfere with the in-flight read. The real
	// close must take the cursor returned by that read, never the old cursor.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Close(ctx, "cancelled-close"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Close(t.Context(), "close"); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil || s.Info().State != "closed" || s.Info().Cursor != 1 {
		t.Fatal(err, s.Info())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "audit"))
	if err != nil || bytes.Count(raw, []byte("plugin.events.close")) != 1 || bytes.Contains(raw, []byte("cancelled-close")) {
		t.Fatal(string(raw), err)
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var request struct {
			Method string     `json:"method"`
			Params EventClose `json:"params"`
		}
		if json.Unmarshal(line, &request) == nil && request.Method == "plugin.events.close" && request.Params.Cursor != 1 {
			t.Fatal("close lost the in-flight read cursor", request)
		}
	}
}
