package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"zende.local/harness/internal/resource/plugin"
)

func EventCapabilities() []plugin.EventCapability {
	return []plugin.EventCapability{
		{Name: "activity", Description: "Receive Core activity", Direction: "core"},
		{Name: "updates", Description: "Read fixture changes", Direction: "plugin", Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`)},
	}
}

type eventState struct {
	open   plugin.EventOpen
	cursor int64
	closed bool
}

type eventFixture map[string]*eventState

func (f eventFixture) handle(method string, raw json.RawMessage, version string) any {
	if path := os.Getenv("ZENDE_TEST_PLUGIN_EVENT_AUDIT"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(12)
		}
		err = json.NewEncoder(file).Encode(map[string]any{"method": method, "params": raw})
		file.Close()
		if err != nil {
			os.Exit(12)
		}
	}
	mode := os.Getenv("ZENDE_TEST_PLUGIN_MODE")
	switch method {
	case "plugin.events.open":
		var p plugin.EventOpen
		if json.Unmarshal(raw, &p) != nil || p.Scope.AgentID == "" || f[p.SubscriptionID] != nil {
			os.Exit(13)
		}
		f[p.SubscriptionID] = &eventState{open: p, cursor: p.After}
		return plugin.EventAck{SubscriptionID: p.SubscriptionID, Cursor: p.After, State: "open"}
	case "plugin.events.read":
		var p plugin.EventRead
		if json.Unmarshal(raw, &p) != nil {
			os.Exit(13)
		}
		s := f[p.SubscriptionID]
		if s == nil || s.closed || s.open.Direction != "plugin" || p.After > s.cursor {
			os.Exit(13)
		}
		if mode == "events-read-exit" && p.After > 0 {
			os.Exit(14)
		}
		page := plugin.PluginEventPage{SubscriptionID: p.SubscriptionID, After: p.After, Next: p.After, Events: []plugin.PluginEvent{}}
		for n := p.After + 1; n <= 3 && len(page.Events) < p.Limit; n++ {
			value, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("private-plugin-%s-event-%d", version, n)})
			page.Events = append(page.Events, plugin.PluginEvent{Sequence: n, Kind: "changed", Value: value})
			page.Next = n
		}
		if len(page.Events) == 0 && p.WaitMS > 0 {
			time.Sleep(time.Duration(min(p.WaitMS, 1000)) * time.Millisecond)
		}
		if mode == "events-source-closed" {
			page.Closed, s.closed = true, true
		}
		s.cursor = max(s.cursor, page.Next)
		return page
	case "plugin.events.deliver":
		var p plugin.CoreEventBatch
		if json.Unmarshal(raw, &p) != nil {
			os.Exit(13)
		}
		s := f[p.SubscriptionID]
		if s == nil || s.closed || s.open.Direction != "core" || p.After != s.cursor {
			os.Exit(13)
		}
		for _, event := range p.Events {
			if event.AgentID != s.open.Scope.AgentID || event.Seq <= p.After || event.Seq > p.Through {
				os.Exit(13)
			}
		}
		s.cursor = p.Through
		if mode == "events-deliver-exit" {
			os.Exit(14)
		}
		return plugin.EventAck{SubscriptionID: p.SubscriptionID, Cursor: p.Through, State: "open"}
	case "plugin.events.close":
		var p plugin.EventClose
		if json.Unmarshal(raw, &p) != nil {
			os.Exit(13)
		}
		s := f[p.SubscriptionID]
		if s == nil || s.closed || p.Cursor != s.cursor {
			os.Exit(13)
		}
		if mode == "events-close-exit" {
			os.Exit(14)
		}
		s.closed = true
		return plugin.EventAck{SubscriptionID: p.SubscriptionID, Cursor: p.Cursor, State: "closed"}
	}
	os.Exit(13)
	return nil
}
