package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func TestPluginSubscriptionDeliveryPreDispatchRejection(t *testing.T) {
	f := newEventJournal(t)
	sub, _, target := openSubscription(t, f, "core", "core", "agent.created")
	batch, err := f.s.BeginPluginEventDelivery(t.Context(), target, "owner")
	if err != nil || batch == nil || len(batch.Events) != 1 || batch.Events[0].Kind != "agent.created" {
		t.Fatal(batch, err)
	}
	for range 2 {
		if err := f.s.RejectPluginEventDelivery(t.Context(), target, "owner", *batch); err != nil {
			t.Fatal(err)
		}
	}
	r, err := f.s.PluginSubscription(t.Context(), target)
	if err != nil || r.Receipt.Cursor != 0 || r.Receipt.Delivery.Pending() || !r.Receipt.Delivery.NotDispatched {
		t.Fatal(r, err)
	}
	wantPluginError(t, f.s.ConfirmPluginEventDelivery(t.Context(), target, "owner", *batch), "operation_conflict")
	next, err := f.s.BeginPluginEventDelivery(t.Context(), target, "owner")
	if err != nil || !reflect.DeepEqual(next, batch) {
		t.Fatal(next, err)
	}
	if err := sub.Deliver(t.Context(), *next); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ConfirmPluginEventDelivery(t.Context(), target, "owner", *next); err != nil {
		t.Fatal(err)
	}
	closeSubscription(t, f, sub, target)
}

func TestPluginSubscriptionReadBoundsEscapedWireBytes(t *testing.T) {
	f := newEventJournal(t)
	_, _, target := openSubscription(t, f, "source", "plugin")
	// Retained raw values fit the plugin budget, but default JSON IPC encoding
	// expands '&' six-fold. Pagination must use the actual encoded byte size.
	value := json.RawMessage(`{"text":"` + strings.Repeat("&", 240<<10) + `"}`)
	for start := int64(0); start < 12; start += 4 {
		page := plugin.PluginEventPage{SubscriptionID: target.SubscriptionID, After: start, Next: start + 4, Events: []plugin.PluginEvent{}}
		for n := start + 1; n <= page.Next; n++ {
			page.Events = append(page.Events, plugin.PluginEvent{Sequence: n, Kind: "changed", Value: value})
		}
		if err := f.s.AppendPluginEvents(t.Context(), target, "owner", page); err != nil {
			t.Fatal(err)
		}
	}
	var after int64
	pages := 0
	for {
		p, err := f.s.ReadPluginEvents(t.Context(), protocol.PluginEventsRead{PluginSubscriptionTarget: target, After: after, Limit: 64})
		if err != nil || p.Next <= after {
			t.Fatal(p.Next, err)
		}
		raw, err := json.Marshal(p)
		if err != nil || len(raw) >= protocol.MaxFrameBytes-4096 {
			t.Fatal("oversized wire page", len(raw), err)
		}
		after = p.Next
		pages++
		if !p.More {
			break
		}
	}
	if after != 12 || pages < 2 {
		t.Fatal(after, pages)
	}
}
