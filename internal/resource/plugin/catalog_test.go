package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPluginCatalogValidationAndReferences(t *testing.T) {
	info := Info{Identity: Identity{ID: "fixture", Version: "1"}, ManifestVersion: "manifest", Capabilities: []string{"tools", "commands"}}
	base := Callable{Name: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)}
	build := func(tools, commands []Callable) (*Catalog, error) {
		raw, _ := json.Marshal(map[string]any{"tools": tools, "commands": commands})
		return compileCatalog(info, raw)
	}
	first, err := build([]Callable{base}, nil)
	if err != nil {
		t.Fatal(err)
	}
	extra := base
	extra.Name = "another"
	second, err := build([]Callable{extra, base}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version() == second.Version() || first.Definitions()[0].Ref != second.Definitions()[1].Ref {
		t.Fatal("unrelated addition invalidated original reference")
	}
	ordered := base
	ordered.InputSchema = json.RawMessage(`{"properties":{"value":{"type":"string"}},"type":"object"}`)
	third, err := build([]Callable{ordered}, nil)
	if err != nil || third.Version() != first.Version() {
		t.Fatal("JSON order changed reference", err)
	}
	changed := base
	changed.Description = "New description"
	fourth, err := build([]Callable{changed}, nil)
	if err != nil || fourth.Definitions()[0].Ref == first.Definitions()[0].Ref {
		t.Fatal("changed definition kept reference", err)
	}
	for name, mutate := range map[string]func(*Callable){
		"name": func(d *Callable) { d.Name = "with space" }, "description": func(d *Callable) { d.Description = "" },
		"schema": func(d *Callable) {
			d.InputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"invalid"}}}`)
		},
		"draft": func(d *Callable) {
			d.InputSchema = json.RawMessage(`{"type":"object","$schema":"https://example.invalid/draft"}`)
		},
		"array":             func(d *Callable) { d.InputSchema = json.RawMessage(`{"type":"array"}`) },
		"output":            func(d *Callable) { d.OutputSchema = json.RawMessage(`null`) },
		"definition-budget": func(d *Callable) { d.Description = strings.Repeat("x", MaxDefinitionBytes) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := base
			mutate(&bad)
			_, err := build([]Callable{bad}, nil)
			requireFailure(t, err, "plugin_protocol")
		})
	}
	_, err = build([]Callable{base, base}, nil)
	requireFailure(t, err, "plugin_protocol")
	raw, _ := json.Marshal(map[string]any{"tools": []Callable{base}, "unknown": true})
	_, err = compileCatalog(info, raw)
	requireFailure(t, err, "plugin_protocol")
	info.Capabilities = []string{"commands"}
	_, err = build([]Callable{base}, nil)
	requireFailure(t, err, "plugin_protocol")
}

func TestPluginSchemasCannotLoadExternalResources(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, ref := range []string{server.URL + "/schema", "file:///nonexistent-plugin-schema"} {
		raw, _ := json.Marshal(map[string]any{"type": "object", "$ref": ref})
		if _, err := compileSchema(raw); err == nil {
			t.Fatal("external schema accepted", ref)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("schema compiler accessed HTTP")
	}
}

func TestPluginJSONBudgetsAndNumericPrecision(t *testing.T) {
	const value = `{"large":123456789012345678901234567890,"exponent":1e999}`
	v, err := decodeValue([]byte(value), MaxArgumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if !strings.Contains(string(raw), "123456789012345678901234567890") || !strings.Contains(string(raw), "1e999") {
		t.Fatal("rounded opaque data", string(raw))
	}
	for _, raw := range []string{`{"x":1e1025}`, `{"x":1e-1025}`, `{"x":1,"x":2}`, `{} {}`, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), strings.Repeat(" ", MaxArgumentBytes+1), string([]byte{255})} {
		if _, err := decodeValue([]byte(raw), MaxArgumentBytes); err == nil {
			t.Fatal("unbounded or ambiguous input accepted")
		}
	}
}
