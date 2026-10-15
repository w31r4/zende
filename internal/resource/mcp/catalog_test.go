package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/protocol"
)

func tool(name string) *mcpsdk.Tool {
	return &mcpsdk.Tool{Name: name, Description: "Find project files", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}
}

func snapshot(t *testing.T, server, version string, tools ...*mcpsdk.Tool) *Snapshot {
	t.Helper()
	if tools == nil {
		tools = []*mcpsdk.Tool{}
	}
	s, err := prepare(t.Context(), server, version, func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
		return &mcpsdk.ListToolsResult{Tools: tools}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func errorKind(t *testing.T, err error, want string) {
	t.Helper()
	var diagnostic *protocol.Error
	if !errors.As(err, &diagnostic) || diagnostic.Data.Kind != want {
		t.Fatalf("error = %v; want %s", err, want)
	}
}

func TestCatalogReferenceBindingAndImmutableCopies(t *testing.T) {
	original := tool("read")
	a := snapshot(t, "local", "config-v1", original)
	result, err := a.Search(SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ref := result.Tools[0].Ref
	original.Description = "changed remotely"
	result.Tools[0].Tool[0] = '!'
	saved, err := a.Lookup(ref)
	if err != nil || !json.Valid(saved.Tool) || strings.Contains(string(saved.Tool), "changed remotely") {
		t.Fatalf("mutable snapshot: %+v %v", saved, err)
	}
	saved.Tool[0] = '!'
	saved, _ = a.Lookup(ref)
	if !json.Valid(saved.Tool) {
		t.Fatal("Lookup exposed snapshot bytes")
	}
	b := snapshot(t, "local", "config-v1", tool("write"), tool("read"))
	if _, err := b.Lookup(ref); err != nil {
		t.Fatal("unrelated addition invalidated reference", err)
	}
	if a.Version() == b.Version() {
		t.Fatal("catalog version did not change")
	}
	for name, next := range map[string]*Snapshot{
		"definition": snapshot(t, "local", "config-v1", original),
		"removed":    snapshot(t, "local", "config-v1"),
		"binding":    snapshot(t, "local", "config-v2", tool("read")),
		"server":     snapshot(t, "other", "config-v1", tool("read")),
	} {
		t.Run(name, func(t *testing.T) { _, err := next.Lookup(ref); errorKind(t, err, "stale_reference") })
	}
	// Object order and server page order do not affect identity.
	reordered := tool("read")
	reordered.InputSchema = json.RawMessage(`{"required":["path"],"properties":{"path":{"type":"string"}},"type":"object"}`)
	if got := snapshot(t, "local", "config-v1", reordered, tool("write")); got.Version() != b.Version() {
		t.Fatal("order changed version")
	}
}

func TestCatalogPersistenceAndPublishedSearchBudget(t *testing.T) {
	s := snapshot(t, "local", "config", tool("one"), tool("two"))
	data := s.Export()
	restored, err := Restore(t.Context(), "local", "config", data)
	if err != nil || restored.Version() != s.Version() {
		t.Fatal(restored, err)
	}
	for _, change := range []func(*Catalog){
		func(c *Catalog) { c.Version = "wrong" },
		func(c *Catalog) { c.Tools[0].Ref = "wrong" },
		func(c *Catalog) { c.Tools[0].ServerID = "other" },
		func(c *Catalog) { c.Tools[0].Tool = []byte(`{"name":"changed","inputSchema":{"type":"object"}}`) },
	} {
		copy := s.Export()
		change(&copy)
		if _, err := Restore(t.Context(), "local", "config", copy); err == nil {
			t.Fatal("corrupt saved catalog accepted")
		}
	}
	if _, err := Restore(t.Context(), "local", "other-binding", data); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	data.Tools[0].Tool[0] = '!'
	if _, err := Restore(t.Context(), "local", "config", s.Export()); err != nil {
		t.Fatal("export aliases immutable bytes")
	}
	for _, budget := range []int{1024, 1200, 2048} {
		result, err := s.SearchPublished(SearchRequest{MaxBytes: budget}, strings.Repeat("a", 64), 99)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(result)
		if len(b) > budget || result.Generation != 99 || result.ResourceVersion != strings.Repeat("a", 64) {
			t.Fatal(len(b), result)
		}
	}
}

func TestCatalogPaginationAndFailureAreAllOrNothing(t *testing.T) {
	var calls []string
	list := func(_ context.Context, cursor string) (*mcpsdk.ListToolsResult, error) {
		calls = append(calls, cursor)
		if cursor == "" {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("b")}, NextCursor: "next"}, nil
		}
		return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("a")}}, nil
	}
	s, err := prepare(t.Context(), "s", "v", list)
	if err != nil || s.Len() != 2 || strings.Join(calls, ",") != ",next" {
		t.Fatalf("%+v %v %v", s, err, calls)
	}
	if s.Version() != snapshot(t, "s", "v", tool("a"), tool("b")).Version() {
		t.Fatal("page order changed version")
	}
	for _, duplicate := range []bool{false, true} {
		s, err := prepare(t.Context(), "s", "v", func(_ context.Context, cursor string) (*mcpsdk.ListToolsResult, error) {
			if cursor == "" {
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("a")}, NextCursor: "next"}, nil
			}
			if duplicate {
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{tool("a")}}, nil
			}
			return nil, errors.New("page two failed")
		})
		if err == nil || s != nil {
			t.Fatalf("partial catalog escaped: %v %v", s, err)
		}
	}
}

func TestCatalogRejectsMalformedDefinitionsAndSchemas(t *testing.T) {
	tests := map[string]*mcpsdk.Tool{
		"nil":        nil,
		"name":       {Name: "bad\nname", InputSchema: map[string]any{"type": "object"}},
		"empty name": {InputSchema: map[string]any{"type": "object"}},
		"long name":  tool(strings.Repeat("x", 129)),
	}
	for name, schema := range map[string]string{
		"missing": "null", "boolean": "true", "array": `{"type":"array"}`,
		"bad keyword":        `{"type":"object","required":"x"}`,
		"bad regexp":         `{"type":"object","properties":{"x":{"pattern":"["}}}`,
		"broken local ref":   `{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}}}`,
		"remote ref":         `{"type":"object","properties":{"x":{"$ref":"https://example.invalid/schema"}}}`,
		"file ref":           `{"type":"object","properties":{"x":{"$ref":"file:///etc/passwd"}}}`,
		"unsupported draft":  `{"type":"object","$schema":"https://example.invalid/draft"}`,
		"null properties":    `{"type":"object","properties":null}`,
		"null schema":        `{"type":"object","additionalProperties":null}`,
		"null required":      `{"type":"object","required":null}`,
		"duplicate required": `{"type":"object","required":["x","x"]}`,
		"negative limit":     `{"type":"object","minProperties":-1}`,
	} {
		value := tool("read")
		value.InputSchema = json.RawMessage(schema)
		tests[name] = value
	}
	badOutput := tool("read")
	badOutput.OutputSchema = map[string]any{"type": "string"}
	tests["output schema"] = badOutput
	for name, value := range tests {
		t.Run(name, func(t *testing.T) { _, err := prepareEntry("s", "v", value); errorKind(t, err, "invalid_resource") })
	}
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{"x":{"$ref":"#/$defs/item"}},"$defs":{"item":{"type":"string"}}}`,
		`{"type":"object","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false}`,
		`{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"x":{"type":["string","null"]}}}`,
	} {
		value := tool("read")
		value.InputSchema = json.RawMessage(schema)
		if _, err := prepareEntry("s", "v", value); err != nil {
			t.Fatalf("supported schema %s: %v", schema, err)
		}
	}
}

func TestCatalogPreparationBounds(t *testing.T) {
	for name, list := range map[string]listTools{
		"nil page":      func(context.Context, string) (*mcpsdk.ListToolsResult, error) { return nil, nil },
		"missing tools": func(context.Context, string) (*mcpsdk.ListToolsResult, error) { return &mcpsdk.ListToolsResult{}, nil },
		"cursor cycle": func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{}, NextCursor: "same"}, nil
		},
		"cursor size": func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{}, NextCursor: strings.Repeat("a", MaxCursorBytes+1)}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := prepare(t.Context(), "s", "v", list)
			errorKind(t, err, "invalid_resource")
		})
	}
	t.Run("pages", func(t *testing.T) {
		calls := 0
		_, err := prepare(t.Context(), "s", "v", func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			calls++
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{}, NextCursor: fmt.Sprint(calls)}, nil
		})
		errorKind(t, err, "resource_limit")
		if calls != MaxPages {
			t.Fatal(calls)
		}
	})
	t.Run("count", func(t *testing.T) {
		_, err := prepare(t.Context(), "s", "v", func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: make([]*mcpsdk.Tool, MaxTools+1)}, nil
		})
		errorKind(t, err, "resource_limit")
	})
	t.Run("definition bytes", func(t *testing.T) {
		value := tool("large")
		value.Description = strings.Repeat("x", MaxDefinitionBytes)
		_, err := prepareEntry("s", "v", value)
		errorKind(t, err, "resource_limit")
	})
	t.Run("catalog bytes", func(t *testing.T) {
		tools := []*mcpsdk.Tool{}
		for i := 0; i < 70; i++ {
			value := tool(fmt.Sprint(i))
			value.Description = strings.Repeat("x", 250<<10)
			tools = append(tools, value)
		}
		_, err := prepare(t.Context(), "s", "v", func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: tools}, nil
		})
		errorKind(t, err, "resource_limit")
	})
	t.Run("depth", func(t *testing.T) {
		value := tool("deep")
		value.InputSchema = json.RawMessage(`{"type":"object","x-extra":` + strings.Repeat("[", MaxJSONDepth) + "0" + strings.Repeat("]", MaxJSONDepth) + "}")
		_, err := prepareEntry("s", "v", value)
		errorKind(t, err, "invalid_resource")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := prepare(ctx, "s", "v", func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
			t.Fatal("listed after cancellation")
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestSearchBudgetCursorAndRanking(t *testing.T) {
	tools := []*mcpsdk.Tool{}
	for i := range 12 {
		tools = append(tools, tool(fmt.Sprintf("tool-%02d", i)))
	}
	s := snapshot(t, "s", "v", tools...)
	first, err := s.Search(SearchRequest{Query: "project", Limit: 3})
	if err != nil || len(first.Tools) != 3 || first.NextCursor == "" {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := s.Search(SearchRequest{Query: "  PROJECT ", Cursor: first.NextCursor, Limit: 20})
	if err != nil || len(second.Tools) != 9 || second.NextCursor != "" || first.Tools[2].Ref == second.Tools[0].Ref {
		t.Fatalf("%+v %v", second, err)
	}
	_, err = s.Search(SearchRequest{Query: "other", Cursor: first.NextCursor})
	errorKind(t, err, "stale_cursor")
	next := snapshot(t, "s", "v", append(tools, tool("extra"))...)
	_, err = next.Search(SearchRequest{Query: "project", Cursor: first.NextCursor})
	errorKind(t, err, "stale_cursor")
	for _, req := range []SearchRequest{{Limit: -1}, {Limit: 51}, {MaxBytes: 1}, {MaxBytes: MaxSearchBytes + 1}, {Query: strings.Repeat("x", MaxQueryBytes+1)}} {
		_, err := s.Search(req)
		errorKind(t, err, "invalid_params")
	}
	_, err = s.Search(SearchRequest{Cursor: "garbage"})
	errorKind(t, err, "stale_cursor")
	for _, budget := range []int{1024, 1500, 2048, 4096} {
		result, err := s.Search(SearchRequest{MaxBytes: budget, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(result)
		if len(b) > budget || len(result.Tools) == 0 {
			t.Fatalf("budget %d result %d bytes", budget, len(b))
		}
	}
	large := tool("large")
	large.Description = strings.Repeat("x", 2000)
	largeSnapshot := snapshot(t, "s", "v", large)
	_, err = largeSnapshot.Search(SearchRequest{MaxBytes: 1024})
	errorKind(t, err, "budget_exceeded")
	if result, err := largeSnapshot.Search(SearchRequest{MaxBytes: 4096}); err != nil || len(result.Tools) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	result, err := s.Search(SearchRequest{Query: "nothing_matches"})
	if err != nil || len(result.Tools) != 0 || result.Tools == nil {
		t.Fatalf("%+v %v", result, err)
	}
	match := tool("project")
	ranked := snapshot(t, "s", "v", tool("a"), match)
	result, err = ranked.Search(SearchRequest{Query: "project"})
	if err != nil || !strings.Contains(string(result.Tools[0].Tool), `"name":"project"`) {
		t.Fatalf("%+v %v", result, err)
	}
}
