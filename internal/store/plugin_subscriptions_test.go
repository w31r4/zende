package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

// These drops only reconstruct old schemas in disposable migration fixtures.
const dropPluginSubscriptionSchema = `DROP VIEW plugin_subscription_heads; DROP TABLE plugin_subscription_events; DROP TABLE plugin_subscription_history; DROP TABLE plugin_subscriptions; `

func newEventJournal(t *testing.T) *pluginJournalFixture {
	t.Helper()
	f := newPluginJournal(t, "")
	f.manifest.Capabilities = []string{"events"}
	f.publish(t, "2")
	return f
}

func prepareSubscription(t *testing.T, f *pluginJournalFixture, id, direction string, kinds ...string) (*plugin.EventSubscription, PluginSubscriptionIntent, protocol.PluginSubscriptionTarget) {
	t.Helper()
	var ref string
	for _, d := range f.pkg.Plugin.Events {
		if d.Direction == direction {
			ref = d.Ref
		}
	}
	p := protocol.PluginSubscribe{AgentID: f.agent, ResourceID: f.pkg.Info.ID, DefinitionRef: ref, Kinds: kinds}
	req, _ := protocol.NewRequest("plugin.events.subscribe", id, p)
	scope, err := f.s.PluginSubscriptionScope(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := f.instance.PrepareEvents(plugin.EventSubscribe{OperationID: id, SubscriptionID: id, DefinitionRef: ref, Scope: scope, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	i := PluginSubscriptionIntent{Request: req, Binding: sub.Info(), ResourceVersion: f.pkg.Info.Version, Generation: f.generation}
	return sub, i, protocol.PluginSubscriptionTarget{AgentID: f.agent, SubscriptionID: id}
}

func openSubscription(t *testing.T, f *pluginJournalFixture, id, direction string, kinds ...string) (*plugin.EventSubscription, PluginSubscriptionIntent, protocol.PluginSubscriptionTarget) {
	t.Helper()
	sub, i, target := prepareSubscription(t, f, id, direction, kinds...)
	r, fresh, err := f.s.BeginPluginSubscription(t.Context(), i, "owner")
	if err != nil || !fresh || r.Receipt.State != "opening" {
		t.Fatal(r, fresh, err)
	}
	if err := sub.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.OpenPluginSubscription(t.Context(), target, "owner", sub.Info()); err != nil {
		t.Fatal(err)
	}
	return sub, i, target
}

func closeSubscription(t *testing.T, f *pluginJournalFixture, sub *plugin.EventSubscription, target protocol.PluginSubscriptionTarget) {
	t.Helper()
	req, _ := protocol.NewRequest("plugin.events.close", "close-"+target.SubscriptionID, target)
	if _, err := f.s.ClosePluginSubscription(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(t.Context(), req.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ConfirmPluginSubscriptionClosed(t.Context(), target, "owner", sub.Info()); err != nil {
		t.Fatal(err)
	}
}

func TestPluginSubscriptionsBothDirectionsRetainSourceAndClose(t *testing.T) {
	f := newEventJournal(t)
	before, _ := os.ReadFile(filepath.Join(filepath.Dir(f.path), "audit.jsonl"))
	source, intent, target := prepareSubscription(t, f, "source", "plugin")
	after, _ := os.ReadFile(filepath.Join(filepath.Dir(f.path), "audit.jsonl"))
	if !bytes.Equal(before, after) {
		t.Fatal("preparation dispatched an event request")
	}
	r, fresh, err := f.s.BeginPluginSubscription(t.Context(), intent, "owner")
	if err != nil || !fresh || r.Receipt.State != "opening" || r.Receipt.ResourceVersion != f.pkg.Info.Version {
		t.Fatal(r, fresh, err)
	}
	if err := source.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.OpenPluginSubscription(t.Context(), target, "owner", source.Info()); err != nil {
		t.Fatal(err)
	}
	page, err := source.Read(t.Context(), 0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Close arrives while a previously read page has yet to be committed.
	req, _ := protocol.NewRequest("plugin.events.close", "close-source", target)
	if _, err := f.s.ClosePluginSubscription(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Close(t.Context(), req.OperationID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.s.ConfirmPluginSubscriptionClosed(t.Context(), target, "owner", source.Info()); err != nil {
			t.Fatal(err)
		}
	}
	r, err = f.s.PluginSubscription(t.Context(), target)
	if err != nil || r.Receipt.State != "closed" || r.Receipt.Cursor != 2 || r.Receipt.InstanceID != source.Info().InstanceID || r.Receipt.CloseOperationID != req.OperationID {
		t.Fatal(r, err)
	}
	events, err := f.s.ReadPluginEvents(t.Context(), protocol.PluginEventsRead{PluginSubscriptionTarget: target, Limit: 1})
	if err != nil || !events.More || events.Next != 1 || !reflect.DeepEqual(events.Events, page.Events[:1]) {
		t.Fatal(events, err)
	}
	core, err := f.s.Events(t.Context(), protocol.Cursor{AgentID: f.agent, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(core)
	if bytes.Contains(raw, []byte("private-plugin-")) {
		t.Fatal("plugin data leaked into Core state events")
	}
	listener, _, listenerTarget := openSubscription(t, f, "listener", "core")
	snapshot, _ := f.s.Snapshot(t.Context(), f.agent)
	batch, err := f.s.BeginPluginEventDelivery(t.Context(), listenerTarget, "owner")
	if err != nil || batch == nil || batch.After != 0 || batch.Through != snapshot.Agent.Seq {
		t.Fatal(batch, err)
	}
	original, err := f.s.Events(t.Context(), protocol.Cursor{AgentID: f.agent})
	if err != nil || !reflect.DeepEqual(batch.Events, original.Events) {
		t.Fatal(batch, original, err)
	}
	if err := listener.Deliver(t.Context(), *batch); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.s.ConfirmPluginEventDelivery(t.Context(), listenerTarget, "owner", *batch); err != nil {
			t.Fatal(err)
		}
	}
	next, err := f.s.BeginPluginEventDelivery(t.Context(), listenerTarget, "owner")
	if err != nil || next != nil {
		t.Fatal("delivery acknowledgement caused a feedback loop", next, err)
	}
	closeSubscription(t, f, listener, listenerTarget)
	intent.Request.ID = "new-transport"
	intent.Binding.InstanceID = "changed-live-process"
	again, fresh, err := f.s.BeginPluginSubscription(t.Context(), intent, "another-owner")
	if err != nil || fresh || !reflect.DeepEqual(again, r) {
		t.Fatal("accepted request was rebound", again, fresh, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.ReadPluginEvents(t.Context(), protocol.PluginEventsRead{PluginSubscriptionTarget: target, Limit: 1})
	if err != nil || !reflect.DeepEqual(saved, events) {
		t.Fatal(saved, err)
	}
}

func TestPluginSubscriptionAcceptanceIsAtomicAndDeduplicated(t *testing.T) {
	for _, point := range []string{"intent", "history", "event", "acceptance"} {
		t.Run(point, func(t *testing.T) {
			f := newEventJournal(t)
			_, i, target := prepareSubscription(t, f, "sub", "plugin")
			before, _ := f.s.Snapshot(t.Context(), f.agent)
			statements := map[string]string{
				"intent":     `CREATE TRIGGER inject BEFORE INSERT ON plugin_subscriptions BEGIN SELECT RAISE(ABORT,'injected'); END`,
				"history":    `CREATE TRIGGER inject BEFORE INSERT ON plugin_subscription_history BEGIN SELECT RAISE(ABORT,'injected'); END`,
				"event":      `CREATE TRIGGER inject BEFORE INSERT ON events WHEN NEW.kind='plugin.subscription.opening' BEGIN SELECT RAISE(ABORT,'injected'); END`,
				"acceptance": `CREATE TRIGGER inject BEFORE INSERT ON operations WHEN NEW.id='sub' BEGIN SELECT RAISE(ABORT,'injected'); END`,
			}
			if _, err := f.s.db.Exec(statements[point]); err != nil {
				t.Fatal(err)
			}
			if r, fresh, err := f.s.BeginPluginSubscription(t.Context(), i, "owner"); err == nil || fresh || r != nil {
				t.Fatal(r, fresh, err)
			}
			after, _ := f.s.Snapshot(t.Context(), f.agent)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed acceptance changed Agent history")
			}
			if r, err := f.s.PluginSubscription(t.Context(), target); err != nil || r != nil {
				t.Fatal(r, err)
			}
			if _, found, err := f.s.AcceptedRequest(t.Context(), i.Request); err != nil || found {
				t.Fatal(found, err)
			}
			if _, err := f.s.db.Exec(`DROP TRIGGER inject`); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make(chan bool, 8)
			for range 8 {
				wg.Go(func() {
					_, fresh, err := f.s.BeginPluginSubscription(t.Context(), i, "owner")
					if err != nil {
						t.Error(err)
					}
					results <- fresh
				})
			}
			wg.Wait()
			close(results)
			count := 0
			for fresh := range results {
				if fresh {
					count++
				}
			}
			if count != 1 {
				t.Fatal("dispatch permits", count)
			}
		})
	}
}

func TestPluginSubscriptionPageAndClosureTransactions(t *testing.T) {
	f := newEventJournal(t)
	sub, _, target := openSubscription(t, f, "sub", "plugin")
	page, err := sub.Read(t.Context(), 0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []string{"data", "history", "event"} {
		statement := `CREATE TRIGGER inject BEFORE INSERT ON plugin_subscription_events BEGIN SELECT RAISE(ABORT,'injected'); END`
		if point == "history" {
			statement = `CREATE TRIGGER inject BEFORE INSERT ON plugin_subscription_history BEGIN SELECT RAISE(ABORT,'injected'); END`
		} else if point == "event" {
			statement = `CREATE TRIGGER inject BEFORE INSERT ON events WHEN NEW.kind='plugin.subscription.received' BEGIN SELECT RAISE(ABORT,'injected'); END`
		}
		if _, err := f.s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err == nil {
			t.Fatal("injected failure committed", point)
		}
		r, _ := f.s.PluginSubscription(t.Context(), target)
		var count int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM plugin_subscription_events`).Scan(&count); err != nil || count != 0 || r.Receipt.Cursor != 0 || r.Receipt.ReceivedBytes != 0 {
			t.Fatal(r, count, err)
		}
		if _, err := f.s.db.Exec(`DROP TRIGGER inject`); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("plugin.events.close", "close-sub", target)
	if _, err := f.s.db.Exec(`CREATE TRIGGER inject BEFORE INSERT ON events WHEN NEW.kind='plugin.subscription.closing' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ClosePluginSubscription(t.Context(), req); err == nil {
		t.Fatal("uncommitted close accepted")
	}
	r, _ := f.s.PluginSubscription(t.Context(), target)
	if r.Receipt.CloseOperationID != "" || r.Receipt.State != "open" {
		t.Fatal(r)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER inject`); err != nil {
		t.Fatal(err)
	}
	closeSubscription(t, f, sub, target)
	for _, stmt := range []string{
		`UPDATE plugin_subscriptions SET owner='replacement'`, `DELETE FROM plugin_subscriptions`,
		`UPDATE plugin_subscription_history SET payload='{}'`, `DELETE FROM plugin_subscription_history`,
		`UPDATE plugin_subscription_events SET payload='{}'`, `DELETE FROM plugin_subscription_events`,
	} {
		if _, err := f.s.db.Exec(stmt); err == nil {
			t.Fatal("history guard allowed", stmt)
		}
	}
}

func TestPluginSubscriptionRecoveryRetainsUnknownDelivery(t *testing.T) {
	f := newEventJournal(t)
	sub, i, target := openSubscription(t, f, "listener", "core")
	batch, err := f.s.BeginPluginEventDelivery(t.Context(), target, "owner")
	if err != nil || batch == nil {
		t.Fatal(batch, err)
	}
	if err := sub.Deliver(t.Context(), *batch); err != nil {
		t.Fatal(err)
	}
	// Plugin accepted the batch but Core crashed before retaining its ack.
	_, err = f.s.BeginPluginEventDelivery(t.Context(), target, "owner")
	wantPluginError(t, err, "operation_unknown")
	for range 2 {
		if err := f.s.InterruptPluginSubscriptions(t.Context(), "replacement"); err != nil {
			t.Fatal(err)
		}
	}
	r, err := f.s.PriorPluginSubscription(t.Context(), i.Request)
	if err != nil || r.Receipt.State != "unknown" || r.Receipt.Cursor != 0 || r.Receipt.Delivery == nil || r.Receipt.Delivery.Confirmed || r.Receipt.Delivery.Through != batch.Through {
		t.Fatal(r, err)
	}
	wantPluginError(t, f.s.ConfirmPluginEventDelivery(t.Context(), target, "owner", *batch), "operation_conflict")
	if _, fresh, err := f.s.BeginPluginSubscription(t.Context(), i, "replacement"); err != nil || fresh {
		t.Fatal(fresh, err)
	}
	i.Request.OperationID = "new-operation"
	_, err = f.s.PluginSubscriptionScope(t.Context(), i.Request)
	wantPluginError(t, err, "operation_unknown")
	var count int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM events WHERE kind='plugin.subscription.unknown'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestPluginSubscriptionRejectsBindingsPagesAndForeignReaders(t *testing.T) {
	f := newEventJournal(t)
	sub, i, target := prepareSubscription(t, f, "sub", "plugin")
	for _, change := range []func(*PluginSubscriptionIntent){
		func(i *PluginSubscriptionIntent) { i.Binding.PluginID = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.PluginVersion = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.ManifestVersion = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.CatalogVersion = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.Scope.Cwd = "/other" },
		func(i *PluginSubscriptionIntent) { i.Binding.Scope.TaskID = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.Direction = "core" },
		func(i *PluginSubscriptionIntent) { i.Binding.Name = "other" },
		func(i *PluginSubscriptionIntent) { i.Binding.State = "open" },
		func(i *PluginSubscriptionIntent) { i.Binding.Cursor = 1 },
		func(i *PluginSubscriptionIntent) { i.Binding.After = 1 },
		func(i *PluginSubscriptionIntent) { i.Generation++ },
		func(i *PluginSubscriptionIntent) { i.ResourceVersion = "other" },
	} {
		bad := i
		change(&bad)
		if _, fresh, err := f.s.BeginPluginSubscription(t.Context(), bad, "owner"); err == nil || fresh {
			t.Fatal("forged binding accepted", bad, err)
		}
	}
	if _, _, err := f.s.BeginPluginSubscription(t.Context(), i, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := sub.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.OpenPluginSubscription(t.Context(), target, "owner", sub.Info()); err != nil {
		t.Fatal(err)
	}
	page, err := sub.Read(t.Context(), 0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*plugin.PluginEventPage){
		func(p *plugin.PluginEventPage) { p.SubscriptionID = "other" },
		func(p *plugin.PluginEventPage) { p.After = 1; p.Next = 3 },
		func(p *plugin.PluginEventPage) { p.Next++ },
		func(p *plugin.PluginEventPage) { p.Events[0].Sequence++ },
		func(p *plugin.PluginEventPage) { p.Events[0].Kind = "bad\nkind" },
		func(p *plugin.PluginEventPage) { p.Events[0].Value = json.RawMessage(`{"text":123}`) },
		func(p *plugin.PluginEventPage) { p.Events[0].Value = json.RawMessage(`{"text":"a","text":"b"}`) },
	} {
		bad := page
		bad.Events = append([]plugin.PluginEvent(nil), page.Events...)
		change(&bad)
		if err := f.s.AppendPluginEvents(t.Context(), target, "owner", bad); err == nil {
			t.Fatal("invalid event page accepted", bad)
		}
	}
	if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err != nil {
		t.Fatal(err)
	}
	page.Events[0].Value = json.RawMessage(`{"text":"rewritten"}`)
	wantPluginError(t, f.s.AppendPluginEvents(t.Context(), target, "owner", page), "operation_conflict")
	query := protocol.PluginEventsRead{PluginSubscriptionTarget: target, After: 3}
	_, err = f.s.ReadPluginEvents(t.Context(), query)
	wantPluginError(t, err, "cursor_gap")
	query.After, query.AgentID = 0, "other"
	_, err = f.s.ReadPluginEvents(t.Context(), query)
	wantPluginError(t, err, "not_found")
	closeSubscription(t, f, sub, target)
}

func TestPluginSubscriptionRetainedLimitsAndTerminalReserve(t *testing.T) {
	f := newEventJournal(t)
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			_, _, target := openSubscription(t, f, mode, "plugin")
			text := "retained"
			if mode == "bytes" {
				text = strings.Repeat("x", plugin.MaxEventBytes-11)
			}
			value, _ := json.Marshal(map[string]string{"text": text})
			var after int64
			for {
				page := plugin.PluginEventPage{SubscriptionID: target.SubscriptionID, After: after, Next: after}
				for range 8 {
					page.Next++
					page.Events = append(page.Events, plugin.PluginEvent{Sequence: page.Next, Kind: "changed", Value: value})
				}
				err := f.s.AppendPluginEvents(t.Context(), target, "owner", page)
				if err != nil {
					if mode == "count" {
						wantPluginError(t, err, "invalid_params")
						if after != MaxPluginSubscriptionEvents {
							t.Fatal(after)
						}
					} else {
						wantPluginError(t, err, "resource_limit")
						if after < 120 {
							t.Fatal(after)
						}
					}
					break
				}
				after = page.Next
			}
			r, err := f.s.PluginSubscription(t.Context(), target)
			if err != nil || r.Receipt.Cursor != after || r.Receipt.ReceivedBytes > MaxPluginSubscriptionBytes {
				t.Fatal(r, err)
			}
			last, err := f.s.ReadPluginEvents(t.Context(), protocol.PluginEventsRead{PluginSubscriptionTarget: target, After: after - 1, Limit: 1})
			if err != nil || last.More || last.Next != after || len(last.Events) != 1 {
				t.Fatal(last, err)
			}
		})
	}
	sub, _, target := openSubscription(t, f, "reserve", "plugin")
	r, _ := f.s.PluginSubscription(t.Context(), target)
	raw, _ := json.Marshal(r.head)
	if _, err := f.s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT ? UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO plugin_subscription_history SELECT ?,x,?,? FROM n`, r.revision+1, MaxPluginSubscriptionChanges, target.SubscriptionID, string(raw), pluginDigest(raw)); err != nil {
		t.Fatal(err)
	}
	r, _ = f.s.PluginSubscription(t.Context(), target)
	if !r.SubscriptionHistoryFull() || r.PluginEventReadLimit() != 0 {
		t.Fatal(r)
	}
	if err := f.s.RequestPluginSubscriptionClose(t.Context(), target, "owner", "resource_limit"); err != nil {
		t.Fatal(err)
	}
	page, err := sub.Read(t.Context(), 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err != nil {
		t.Fatal(err)
	}
	r, _ = f.s.PluginSubscription(t.Context(), target)
	if err := sub.Close(t.Context(), r.Receipt.CloseOperationID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ConfirmPluginSubscriptionClosed(t.Context(), target, "owner", sub.Info()); err != nil {
		t.Fatal(err)
	}
	r, _ = f.s.PluginSubscription(t.Context(), target)
	if r.revision != MaxPluginSubscriptionChanges+3 || r.Receipt.State != "closed" || r.Receipt.Error.Data.Kind != "resource_limit" {
		t.Fatal(r)
	}
}

func TestPluginSubscriptionListingAndSchemaFifteenMigration(t *testing.T) {
	f := newEventJournal(t)
	before, _ := f.s.Snapshot(t.Context(), f.agent)
	if _, err := f.s.db.Exec(dropPluginSubscriptionSchema + `PRAGMA user_version=15;`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.s = s
	after, _ := s.Snapshot(t.Context(), f.agent)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed old history")
	}
	for n := range 3 {
		sub, _, target := openSubscription(t, f, fmt.Sprintf("sub-%d", n), "plugin")
		closeSubscription(t, f, sub, target)
	}
	page, err := s.PluginSubscriptions(t.Context(), protocol.PluginSubscriptionQuery{AgentID: f.agent, ResourceID: f.pkg.Info.ID, Limit: 2})
	if err != nil || len(page.Items) != 2 || !page.More || page.Next != "sub-1" {
		t.Fatal(page, err)
	}
	page, err = s.PluginSubscriptions(t.Context(), protocol.PluginSubscriptionQuery{AgentID: f.agent, After: page.Next, Limit: 2})
	if err != nil || len(page.Items) != 1 || page.More || page.Next != "sub-2" {
		t.Fatal(page, err)
	}
	page, err = s.PluginSubscriptions(t.Context(), protocol.PluginSubscriptionQuery{AgentID: f.agent, ResourceID: "different"})
	if err != nil || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
}

func TestPluginSubscriptionMigrationRollback(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.db.Exec(dropPluginSubscriptionSchema + `PRAGMA user_version=15; CREATE TABLE plugin_subscription_history (blocker TEXT);`); err != nil {
		t.Fatal(err)
	}
	if err := s.initialize(); err == nil {
		t.Fatal("conflicting migration succeeded")
	}
	var version, count int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 15 {
		t.Fatal(version, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='plugin_subscriptions'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := s.db.Exec(`DROP TABLE plugin_subscription_history`); err != nil {
		t.Fatal(err)
	}
	if err := s.initialize(); err != nil {
		t.Fatal(err)
	}
}
