package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

func registeredEventPlugin(t *testing.T, mode string) (*Core, protocol.ResourceTarget, plugin.Manifest, string) {
	t.Helper()
	c, agent, m, source := runtimePluginFixture(t)
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	t.Setenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT", filepath.Join(filepath.Dir(source), "events.jsonl"))
	m.Capabilities = []string{"tools", "commands", "events"}
	saveRuntimePlugin(t, source, m)
	req, _ := protocol.NewRequest("resource.register", "register", protocol.RegisterResource{AgentID: agent, Kind: "plugin", Path: source})
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return c, protocol.ResourceTarget{AgentID: agent, ID: v.(protocol.Accepted).Resource.ID}, m, source
}

func eventRequest(t *testing.T, c *Core, target protocol.ResourceTarget, id, direction string, kinds ...string) protocol.Request {
	t.Helper()
	catalog, err := c.Store.PluginCatalog(t.Context(), protocol.PluginCatalogQuery{AgentID: target.AgentID, ID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range catalog.Events {
		if d.Direction == direction {
			req, _ := protocol.NewRequest("plugin.events.subscribe", id, protocol.PluginSubscribe{AgentID: target.AgentID, ResourceID: target.ID, DefinitionRef: d.Ref, Kinds: kinds})
			return req
		}
	}
	t.Fatal("event capability missing", direction)
	return protocol.Request{}
}

func waitSubscription(t *testing.T, c *Core, target protocol.PluginSubscriptionTarget, match func(store.PluginSubscriptionReceipt) bool) store.PluginSubscriptionReceipt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := c.pluginSubscription(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		if match(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription did not reach expected state", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func closeEventSubscription(t *testing.T, c *Core, target protocol.PluginSubscriptionTarget) store.PluginSubscriptionReceipt {
	t.Helper()
	req, _ := protocol.NewRequest("plugin.events.close", "close-"+target.SubscriptionID, target)
	first, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Handle(t.Context(), req)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal(first, second, err)
	}
	return waitSubscription(t, c, target, func(r store.PluginSubscriptionReceipt) bool { return r.State == "closed" })
}

func TestPluginEventsDetachRetryRefreshAndOfflineReads(t *testing.T) {
	c, target, manifest, source := registeredEventPlugin(t, "")
	old := c.pluginSlots[target].instance
	req := eventRequest(t, c, target, "source", "plugin")
	client, detach := context.WithCancel(t.Context())
	v, err := c.Handle(client, req)
	if err != nil {
		t.Fatal(err)
	}
	detach()
	first := v.(store.PluginSubscriptionReceipt)
	sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "source"}
	waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.State == "open" && r.Cursor == 3 })
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry := req
			retry.ID = protocol.NewID("transport")
			v, err := c.Handle(t.Context(), retry)
			if err != nil {
				t.Error(err)
				return
			}
			if r := v.(store.PluginSubscriptionReceipt); r.InstanceID != first.InstanceID || r.State != "open" {
				t.Error(r)
			}
		}()
	}
	wg.Wait()
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	if old.Info().State != "ready" {
		t.Fatal("refresh ended the old subscription")
	}
	next := eventRequest(t, c, target, "new", "plugin")
	v, err = c.Handle(t.Context(), next)
	if err != nil || v.(store.PluginSubscriptionReceipt).InstanceID == first.InstanceID {
		t.Fatal(v, err)
	}
	closed := closeEventSubscription(t, c, sub)
	waitPluginClosed(t, old)
	c.CloseResources()
	newTarget := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "new"}
	final, err := c.pluginSubscription(t.Context(), newTarget)
	if err != nil || final.State != "closed" && final.State != "not_dispatched" {
		t.Fatal("Core shutdown did not settle subscription", final, err)
	}
	v, err = c.Handle(t.Context(), req)
	if err != nil || !reflect.DeepEqual(v, closed) {
		t.Fatal("offline retry changed original subscription", v, err)
	}
	read, _ := protocol.NewRequest("plugin.events.read", "", protocol.PluginEventsRead{PluginSubscriptionTarget: sub, Limit: 2})
	v, err = c.Handle(t.Context(), read)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if !bytes.Contains(raw, []byte("private-plugin-1-event-1")) || bytes.Contains(raw, []byte("private-plugin-2-")) {
		t.Fatal("read changed plugin version", string(raw))
	}
	list, _ := protocol.NewRequest("plugin.events.list", "", protocol.PluginSubscriptionQuery{AgentID: target.AgentID, ResourceID: target.ID})
	v, err = c.Handle(t.Context(), list)
	if err != nil || len(v.(store.PluginSubscriptionsPage).Items) != 2 {
		t.Fatal(v, err)
	}
	audit, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
	if err != nil || bytes.Count(audit, []byte(`"method":"plugin.events.open"`)) > 2 {
		t.Fatal("retry dispatched a new subscription", string(audit), err)
	}
}

func TestPluginEventsCoreFilteringAndNoFeedback(t *testing.T) {
	c, target, _, _ := registeredEventPlugin(t, "")
	req := eventRequest(t, c, target, "listener", "core", "agent.created")
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "listener"}
	r := waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool {
		return r.State == "open" && r.Delivery != nil && r.Delivery.Confirmed
	})
	before, _ := c.Store.Snapshot(t.Context(), target.AgentID)
	time.Sleep(300 * time.Millisecond)
	after, _ := c.Store.Snapshot(t.Context(), target.AgentID)
	if before.Agent.Seq != after.Agent.Seq {
		t.Fatal("delivery acknowledgements feed back into Core events")
	}
	r = waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.Cursor == after.Agent.Seq })
	if r.Delivery.Pending() {
		t.Fatal(r)
	}
	closeEventSubscription(t, c, sub)
	audit, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID})
	count := 0
	var cursor int64
	for _, line := range bytes.Split(bytes.TrimSpace(audit), []byte{'\n'}) {
		var entry struct {
			Method string
			Params json.RawMessage
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Method != "plugin.events.deliver" {
			continue
		}
		var batch plugin.CoreEventBatch
		if err := json.Unmarshal(entry.Params, &batch); err != nil || batch.After != cursor || batch.Through <= cursor {
			t.Fatal(batch, err)
		}
		cursor = batch.Through
		for _, e := range batch.Events {
			if e.Kind != "agent.created" || e.AgentID != target.AgentID || !reflect.DeepEqual(e, original.Events[e.Seq-1]) {
				t.Fatal("filtered event lost its original identity", e)
			}
			count++
		}
	}
	if count != 1 {
		t.Fatal("missing or repeated Core event", count)
	}
}

func TestPluginEventsSourceCloseAndUnknownOutcomes(t *testing.T) {
	for _, mode := range []string{"events-source-closed", "events-read-exit", "events-deliver-exit", "events-close-exit"} {
		t.Run(mode, func(t *testing.T) {
			c, target, _, _ := registeredEventPlugin(t, mode)
			direction := "plugin"
			if mode == "events-deliver-exit" {
				direction = "core"
			}
			req := eventRequest(t, c, target, "source", direction)
			if _, err := c.Handle(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "source"}
			if mode == "events-close-exit" {
				waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.Cursor == 3 })
				close, _ := protocol.NewRequest("plugin.events.close", "close", sub)
				if _, err := c.Handle(t.Context(), close); err != nil {
					t.Fatal(err)
				}
			}
			want := "unknown"
			if mode == "events-source-closed" {
				want = "closed"
			}
			r := waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.State == want })
			if direction == "plugin" && r.Cursor != 3 {
				t.Fatal(r)
			}
			if direction == "core" && (r.Cursor != 0 || !r.Delivery.Pending()) {
				t.Fatal("invented delivery acknowledgement", r)
			}
			c.CloseResources()
			before, _ := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
			for range 2 {
				v, err := c.Handle(t.Context(), req)
				if err != nil || !reflect.DeepEqual(v, r) {
					t.Fatal(v, err)
				}
			}
			after, _ := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
			if !bytes.Equal(before, after) {
				t.Fatal("replayed unknown subscription")
			}
		})
	}
}

func TestPluginEventsCallerSurvivesTaskStopAndRetry(t *testing.T) {
	c, target, _, _ := registeredEventPlugin(t, "")
	req := eventRequest(t, c, target, "source", "plugin")
	var input protocol.PluginSubscribe
	json.Unmarshal(req.Params, &input)
	originalCwd := t.TempDir()
	input.Caller = pluginCommandCaller(t, c, target.AgentID, "shell", originalCwd, false)
	req.Params, _ = json.Marshal(input)
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	first := v.(store.PluginSubscriptionReceipt)
	if first.Source == nil || first.Source.Caller != *input.Caller || first.Scope.Cwd != originalCwd {
		t.Fatal(first)
	}
	sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "source"}
	waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.Cursor == 3 })
	stop, _ := protocol.NewRequest("agent.stop", "stop", protocol.Target{AgentID: target.AgentID})
	if _, err := c.Handle(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.FinishTask(t.Context(), target.AgentID, input.Caller.TaskID, "cancelled", "", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	r, err := c.pluginSubscription(t.Context(), sub)
	if err != nil || r.State != "open" {
		t.Fatal("ordinary stop ended the background subscription", r, err)
	}
	input.Caller = nil
	req.Params, _ = json.Marshal(input)
	v, err = c.Handle(t.Context(), req)
	if err != nil || !reflect.DeepEqual(v, r) {
		t.Fatal("retry replaced original caller", v, err)
	}
	closeEventSubscription(t, c, sub)
}

func TestPluginEventsPersistenceFaultBlocksOtherPluginEntrypoints(t *testing.T) {
	c, target, manifest, source := registeredEventPlugin(t, "")
	pluginTestSQL(t, c, `CREATE TRIGGER reject_subscription_state BEFORE INSERT ON plugin_subscription_history WHEN NEW.sequence>1 BEGIN SELECT RAISE(ABORT,'injected'); END`)
	req := eventRequest(t, c, target, "source", "plugin")
	command, _ := pluginCommandRequest(t, c, target, "command")
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !isFailure(c.pluginEventFailure(target.AgentID), "persistence_failed") {
		if time.Now().After(deadline) {
			t.Fatal("fault marker missing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Refresh restores a live instance but cannot erase the unpersisted outcome.
	manifest.Version = "2"
	saveRuntimePlugin(t, source, manifest)
	refresh, _ := protocol.NewRequest("resource.refresh", "refresh", target)
	if _, err := c.Handle(t.Context(), refresh); err != nil {
		t.Fatal(err)
	}
	fresh := eventRequest(t, c, target, "new", "plugin")
	for _, q := range []protocol.Request{req, fresh, command} {
		if _, err := c.Handle(t.Context(), q); !isFailure(err, "persistence_failed") {
			t.Fatal(q.Method, err)
		}
	}
	c.CloseResources()
	pluginTestSQL(t, c, `DROP TRIGGER reject_subscription_state`)
	replacement := New(c.Store, c.Config)
	defer replacement.CloseResources()
	if err := c.Store.InterruptPluginSubscriptions(t.Context(), replacement.InstanceID); err != nil {
		t.Fatal(err)
	}
	v, err := replacement.Handle(t.Context(), req)
	if err != nil || v.(store.PluginSubscriptionReceipt).State != "unknown" || v.(store.PluginSubscriptionReceipt).Reason != "core_restarted" {
		t.Fatal(v, err)
	}
}

func TestPluginEventsTreeStopIsAtomicAndAgentScoped(t *testing.T) {
	c, root, _, source := registeredEventPlugin(t, "")
	_, runner, _ := remoteRunner(t)
	c.Runner = runner
	child := treeChild(t, c, root.AgentID)
	create := protocol.CreateAgent{Cwd: filepath.Dir(source)}
	q, _ := protocol.NewRequest("agent.create", "other", create)
	other, err := c.Store.Create(t.Context(), q, create, 8)
	if err != nil {
		t.Fatal(err)
	}
	targets := []protocol.ResourceTarget{root}
	for _, agent := range []protocol.Accepted{child, other} {
		q, _ := protocol.NewRequest("resource.register", "register-"+agent.AgentID, protocol.RegisterResource{AgentID: agent.AgentID, Kind: "plugin", Path: source})
		v, err := c.Handle(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, protocol.ResourceTarget{AgentID: agent.AgentID, ID: v.(protocol.Accepted).Resource.ID})
	}
	var subscriptions []protocol.PluginSubscriptionTarget
	for _, target := range targets {
		q := eventRequest(t, c, target, "sub-"+target.AgentID, "plugin")
		if _, err := c.Handle(t.Context(), q); err != nil {
			t.Fatal(err)
		}
		sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: q.OperationID}
		subscriptions = append(subscriptions, sub)
		waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.Cursor == 3 })
	}
	tree, _ := protocol.NewRequest("agent.stop", "tree", protocol.StopAgent{AgentID: root.AgentID, Tree: true})
	pluginTestSQL(t, c, `CREATE TRIGGER fail_tree_subscription BEFORE INSERT ON operations WHEN NEW.id='tree' BEGIN SELECT RAISE(ABORT,'injected rollback'); END`)
	if _, err := c.Handle(t.Context(), tree); err == nil {
		t.Fatal("accepted failed tree stop")
	}
	for _, sub := range subscriptions {
		r, err := c.pluginSubscription(t.Context(), sub)
		if err != nil || r.State != "open" || r.CloseOperationID != "" {
			t.Fatal("close escaped rollback", r, err)
		}
	}
	pluginTestSQL(t, c, `DROP TRIGGER fail_tree_subscription`)
	if _, err := c.Handle(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	for _, sub := range subscriptions[:2] {
		r := waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.State == "closed" })
		if r.Reason != "tree_stop" {
			t.Fatal(r)
		}
	}
	if stage, err := c.advanceTreeStop(t.Context(), "tree"); err != nil || stage != "" {
		t.Fatal(stage, err)
	}
	r, err := c.pluginSubscription(t.Context(), subscriptions[2])
	if err != nil || r.State != "open" {
		t.Fatal("tree stop affected unrelated Agent", r, err)
	}
	closeEventSubscription(t, c, subscriptions[2])
}

func TestPluginEventsUnknownBlocksTreeCompletion(t *testing.T) {
	c, target, _, _ := registeredEventPlugin(t, "events-close-exit")
	_, runner, _ := remoteRunner(t)
	c.Runner = runner
	q := eventRequest(t, c, target, "source", "plugin")
	if _, err := c.Handle(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: "source"}
	waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.Cursor == 3 })
	stop := requestTreeStop(t, c, target.AgentID, "tree")
	waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.State == "unknown" })
	if _, err := c.advanceTreeStop(t.Context(), stop.OperationID); !isFailure(err, "tree_stopping") {
		t.Fatal("unknown closure reported stopped", err)
	}
}

func TestPluginEventsStartupDoesNotReopenUnfinishedSubscription(t *testing.T) {
	c, target, _, _ := registeredEventPlugin(t, "")
	req := eventRequest(t, c, target, "interrupted", "core")
	var input protocol.PluginSubscribe
	json.Unmarshal(req.Params, &input)
	scope, err := c.Store.PluginSubscriptionScope(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	slot, free, err := c.leasePlugin(target)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := slot.instance.PrepareEvents(plugin.EventSubscribe{OperationID: req.OperationID, SubscriptionID: req.OperationID, DefinitionRef: input.DefinitionRef, Scope: scope})
	if err != nil {
		free()
		t.Fatal(err)
	}
	intent := store.PluginSubscriptionIntent{Request: req, Binding: sub.Info(), ResourceVersion: slot.version, Generation: slot.generation}
	if _, fresh, err := c.Store.BeginPluginSubscription(t.Context(), intent, "previous-core"); err != nil || !fresh {
		free()
		t.Fatal(fresh, err)
	}
	free()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	subTarget := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: req.OperationID}
	r := waitSubscription(t, c, subTarget, func(r store.PluginSubscriptionReceipt) bool { return r.State == "unknown" })
	if r.Reason != "core_restarted" || r.Cursor != 0 {
		t.Fatal(r)
	}
	v, err := c.Handle(t.Context(), req)
	if err != nil || !reflect.DeepEqual(v, r) {
		t.Fatal(v, err)
	}
	audit, _ := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"))
	if bytes.Contains(audit, []byte("plugin.events.open")) {
		t.Fatal("startup reopened original subscription")
	}
}

func TestPluginEventsOversizedCoreEventClosesAtConfirmedCursor(t *testing.T) {
	c, target, _, _ := registeredEventPlugin(t, "")
	event, err := c.Store.Append(t.Context(), target.AgentID, "", "fixture.large", map[string]string{"text": strings.Repeat("x", plugin.MaxEventBatchBytes)})
	if err != nil {
		t.Fatal(err)
	}
	req := eventRequest(t, c, target, "listener", "core")
	if _, err := c.Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	sub := protocol.PluginSubscriptionTarget{AgentID: target.AgentID, SubscriptionID: req.OperationID}
	r := waitSubscription(t, c, sub, func(r store.PluginSubscriptionReceipt) bool { return r.State == "closed" })
	if r.Reason != "resource_limit" || r.Cursor >= event.Seq || r.Delivery.Pending() || c.pluginSlots[target].instance.Info().State != "ready" {
		t.Fatal("budget rejection fabricated delivery or faulted the instance", r)
	}
	// The original event remains queryable, with unchanged identity and data.
	page, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: target.AgentID, After: event.Seq - 1, Limit: 1})
	if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], event) {
		t.Fatal("source event was lost", err)
	}
}
