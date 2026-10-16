package plugin

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func eventCapabilities() []EventCapability {
	return []EventCapability{
		{Name: "updates", Description: "Structured plugin updates", Direction: "plugin", Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`)},
		{Name: "activity", Description: "Observe Core activity", Direction: "core"},
	}
}

func TestPluginEventCatalogVersionsAndRestoration(t *testing.T) {
	m, reply := capabilityFixture()
	m.Capabilities = append(m.Capabilities, "events")
	before, err := compileFixture(m, reply)
	if err != nil {
		t.Fatal(err)
	}
	legacyVersion := digest(struct {
		Definitions []Definition         `json:"definitions"`
		Providers   []ProviderDefinition `json:"providers,omitempty"`
		Resources   []ResourceDefinition `json:"resources,omitempty"`
	}{before.Definitions(), before.Providers(), before.Resources()})
	if before.Version() != legacyVersion {
		t.Fatal("event-free catalog version changed")
	}
	reply.Events = eventCapabilities()
	c, err := compileFixture(m, reply)
	if err != nil || len(c.Events()) != 2 || c.Version() == before.Version() || !reflect.DeepEqual(c.Providers(), before.Providers()) {
		t.Fatal(c, err)
	}
	saved := c.Export()
	restored, err := RestoreCatalog(m, saved)
	if err != nil || !reflect.DeepEqual(restored.Export(), saved) {
		t.Fatal(restored, err)
	}
	copy := c.Export()
	copy.Events[1].Schema[0] = '['
	if !reflect.DeepEqual(c.Export(), saved) {
		t.Fatal("event schema aliased catalog")
	}
	reply.Events[0].Schema = json.RawMessage(`{"additionalProperties":false,"properties":{"text":{"type":"string"}},"required":["text"],"type":"object"}`)
	slices.Reverse(reply.Events)
	other, err := compileFixture(m, reply)
	if err != nil || !reflect.DeepEqual(other.Export(), saved) {
		t.Fatal("schema key/order changed event identity", err)
	}
	for _, mutate := range []func(*SavedCatalog){
		func(s *SavedCatalog) { s.Events[0].Ref = "forged" },
		func(s *SavedCatalog) { s.Events[0].Direction = "plugin" },
		func(s *SavedCatalog) { s.Events[1].Schema = json.RawMessage(`{"type":"object"}`) },
		func(s *SavedCatalog) { slices.Reverse(s.Events) },
		func(s *SavedCatalog) { s.Events = s.Events[:1] },
	} {
		copy := c.Export()
		mutate(&copy)
		if _, err := RestoreCatalog(m, copy); err == nil {
			t.Fatal("forged saved event catalog accepted")
		}
	}
}

func TestPluginEventCatalogRejectsInvalidDefinitions(t *testing.T) {
	for name, change := range map[string]func(*Manifest, *catalogReply){
		"undeclared":     func(m *Manifest, _ *catalogReply) { m.Capabilities = []string{"tools", "providers", "resources"} },
		"name":           func(_ *Manifest, r *catalogReply) { r.Events[0].Name = "a/b" },
		"description":    func(_ *Manifest, r *catalogReply) { r.Events[0].Description = strings.Repeat("x", 8193) },
		"blank":          func(_ *Manifest, r *catalogReply) { r.Events[0].Description = " " },
		"direction":      func(_ *Manifest, r *catalogReply) { r.Events[0].Direction = "both" },
		"duplicate":      func(_ *Manifest, r *catalogReply) { r.Events = append(r.Events, r.Events[0]) },
		"missing-schema": func(_ *Manifest, r *catalogReply) { r.Events[0].Schema = nil },
		"core-schema":    func(_ *Manifest, r *catalogReply) { r.Events[1].Schema = json.RawMessage(`{"type":"object"}`) },
		"external-schema": func(_ *Manifest, r *catalogReply) {
			r.Events[0].Schema = json.RawMessage(`{"type":"object","$ref":"https://example.invalid/schema"}`)
		},
		"null-schema":       func(_ *Manifest, r *catalogReply) { r.Events[0].Schema = json.RawMessage(`null`) },
		"definitions-limit": func(_ *Manifest, r *catalogReply) { r.Events = make([]EventCapability, MaxDefinitions) },
	} {
		t.Run(name, func(t *testing.T) {
			m, reply := capabilityFixture()
			m.Capabilities = append(m.Capabilities, "events")
			reply.Events = eventCapabilities()
			change(&m, &reply)
			_, err := compileFixture(m, reply)
			requireFailure(t, err, "plugin_protocol")
		})
	}
}
