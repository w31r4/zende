package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func callRef(t *testing.T, c *Connection, query string) string {
	t.Helper()
	s, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(SearchRequest{Query: query})
	if err != nil || len(page.Tools) != 1 {
		t.Fatal(page, err)
	}
	return page.Tools[0].Ref
}

func TestCallValidatesBeforeDispatchAndPreservesArguments(t *testing.T) {
	server := fixtureServer("2025-11-25")
	var calls atomic.Int32
	var arguments string
	definition := &mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)}
	server.AddTool(definition, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		arguments = string(req.Params.Arguments)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "done"}}}, nil
	})
	c := connectMemory(t, server)
	ref := callRef(t, c, "act")
	for name, raw := range map[string]string{
		"missing": `{}`, "wrong type": `{"n":"secret-value"}`, "extra": `{"n":1,"x":true}`,
		"null": `null`, "array": `[]`, "duplicate": `{"n":1,"n":2}`, "second": `{"n":1}{}`,
		"nested duplicate": `{"n":1,"x":{"v":1,"v":2}}`, "invalid UTF8": "{\"n\":\"\xff\"}",
		"budget": strings.Repeat(" ", MaxArgumentBytes+1), "depth": `{"n":` + strings.Repeat("[", 65) + `1` + strings.Repeat("]", 65) + `}`,
		"number exponent": `{"n":1e100000000}`, "number digits": `{"n":` + strings.Repeat("9", 1025) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.PrepareCall(t.Context(), ref, []byte(raw))
			errorKind(t, err, "invalid_arguments")
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("argument leaked in diagnostic")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments dispatched")
	}
	raw := json.RawMessage(`{"n":9007199254740993}`)
	call, err := c.PrepareCall(t.Context(), ref, raw)
	if err != nil {
		t.Fatal(err)
	}
	definitionCopy := call.Definition()
	definitionCopy.Tool[0] = '!'
	copy := call.Arguments()
	copy[0], raw[0] = '!', '!'
	if !json.Valid(call.Definition().Tool) || !json.Valid(call.Arguments()) {
		t.Fatal("mutable prepared call")
	}
	result, err := call.Invoke(t.Context())
	if err != nil || !strings.Contains(string(result.Result), "done") || result.ToolRef != ref || result.CatalogVersion != call.CatalogVersion() {
		t.Fatal(result, err)
	}
	if arguments != `{"n":9007199254740993}` || calls.Load() != 1 {
		t.Fatal(arguments, calls.Load())
	}
	_, err = call.Invoke(t.Context())
	errorKind(t, err, "operation_conflict")
}

func TestCallResultContentAndOutputSchema(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			server := fixtureServer(version)
			definition := &mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}}}`)}
			var mu sync.Mutex
			reply := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "text", Meta: mcpsdk.Meta{"origin": "fixture"}},
				&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
				&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte{4, 5}},
				&mcpsdk.ResourceLink{Name: "link", URI: "https://example.invalid/data"},
				&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file:///fixture", Text: "data"}},
			}, StructuredContent: map[string]any{"value": 7}, Meta: mcpsdk.Meta{"fixture": true}}
			server.AddTool(definition, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				mu.Lock()
				defer mu.Unlock()
				return reply, nil
			})
			c := connectMemory(t, server)
			ref := callRef(t, c, "act")
			invoke := func() (CallResult, error) {
				call, err := c.PrepareCall(t.Context(), ref, []byte(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				return call.Invoke(t.Context())
			}
			result, err := invoke()
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{`"type":"text"`, `"type":"image"`, `"type":"audio"`, `"type":"resource_link"`, `"type":"resource"`, `"structuredContent":{"value":7}`, `"fixture":true`, `"origin":"fixture"`, `"data":"AQID"`} {
				if !strings.Contains(string(result.Result), fragment) {
					t.Fatal("missing", fragment, string(result.Result))
				}
			}
			mu.Lock()
			reply = &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "failed"}}, IsError: true}
			mu.Unlock()
			result, err = invoke()
			if err != nil || !strings.Contains(string(result.Result), `"isError":true`) {
				t.Fatal(result, err)
			}
			mu.Lock()
			reply = &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "already acted"}}, StructuredContent: map[string]any{"value": "bad"}}
			mu.Unlock()
			result, err = invoke()
			errorKind(t, err, "operation_unknown")
			if !strings.Contains(string(result.Result), "already acted") {
				t.Fatal("lost invalid response evidence")
			}
		})
	}
}

func TestCallCatalogChangesAndInFlightSchema(t *testing.T) {
	server := fixtureServer("2025-11-25")
	started, finish := make(chan struct{}), make(chan struct{})
	definition := &mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"const":"old"}},"required":["value"]}`)}
	server.AddTool(definition, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "old"}}, StructuredContent: map[string]any{"value": "old"}}, nil
	})
	c := connectMemory(t, server)
	ref := callRef(t, c, "act")
	call, err := c.PrepareCall(t.Context(), ref, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := c.PrepareCall(t.Context(), ref, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := call.Invoke(t.Context()); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	next := *definition
	next.OutputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"const":"new"}},"required":["value"]}`)
	addTool(server, &next)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = c.PrepareCall(t.Context(), ref, []byte(`{}`))
	errorKind(t, err, "stale_reference")
	_, err = waiting.Invoke(t.Context())
	errorKind(t, err, "stale_reference")
	close(finish)
	if err := <-done; err != nil {
		t.Fatal("new schema affected running call", err)
	}
	ref = callRef(t, c, "act")
	addTool(server, tool("other"))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareCall(t.Context(), ref, []byte(`{}`)); err != nil {
		t.Fatal("unrelated addition invalidated ref", err)
	}
	server.RemoveTools("act")
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = c.PrepareCall(t.Context(), ref, []byte(`{}`))
	errorKind(t, err, "stale_reference")
}

func TestCallCancellationAndSingleUseDoNotRetry(t *testing.T) {
	server := fixtureServer("2025-11-25")
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	server.AddTool(&mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		signal(started)
		<-ctx.Done()
		return nil, errors.New("secret-remote-error")
	})
	c := connectMemory(t, server)
	ref := callRef(t, c, "act")
	call, err := c.PrepareCall(t.Context(), ref, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := call.Invoke(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	cancel()
	err = <-done
	errorKind(t, err, "operation_unknown")
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("remote diagnostic leaked")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := call.Invoke(t.Context()); errorKind(t, err, "operation_conflict") }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("retried", calls.Load())
	}
	call, err = c.PrepareCall(t.Context(), ref, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = call.Invoke(ctx)
	errorKind(t, err, "cancelled")
	if calls.Load() != 1 {
		t.Fatal("pre-cancelled call dispatched")
	}
}

func TestCallUnrelatedAdditionKeepsReferenceInvokable(t *testing.T) {
	server := fixtureServer("2025-11-25")
	var calls atomic.Int32
	server.AddTool(&mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "original target"}}}, nil
	})
	c := connectMemory(t, server)
	ref := callRef(t, c, "act")
	addTool(server, tool("unrelated"))
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareCall(t.Context(), ref, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Invoke(t.Context())
	if err != nil || calls.Load() != 1 || !strings.Contains(string(result.Result), "original target") {
		t.Fatal(result, err, calls.Load())
	}
}

func TestHeaderValuesAreBoundedAndUnambiguous(t *testing.T) {
	for _, value := range []string{"你好", " x", "x ", "\n", "=?base64?eA==?="} {
		encoded, ok := headerValue(value)
		if !ok || !strings.HasPrefix(encoded, "=?base64?") {
			t.Fatal(value, encoded)
		}
	}
	for raw, want := range map[string]string{"2": "2", "2e2": "200", "9007199254740991": "9007199254740991", "-9": "-9"} {
		got, ok := headerValue(json.Number(raw))
		if !ok || got != want {
			t.Fatal(raw, got)
		}
	}
	for _, raw := range []string{"9007199254740992", "-9007199254740992", "1.1"} {
		if _, ok := headerValue(json.Number(raw)); ok {
			t.Fatal(raw)
		}
	}
	schema := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"key":{"type":"string","x-mcp-header":"Tenant"}}}}}`)
	headers, err := parameterHeaders(schema, map[string]any{"nested": map[string]any{"key": "hello"}})
	if err != nil || headers["Mcp-Param-Tenant"] != "hello" {
		t.Fatal(headers, err)
	}
	_, err = parameterHeaders(schema, map[string]any{"nested": map[string]any{"key": strings.Repeat("x", 16<<10)}})
	errorKind(t, err, "invalid_arguments")
}

func TestCallResultBoundsAndUnsupportedContent(t *testing.T) {
	cases := []struct {
		name     string
		reply    func() *mcpsdk.CallToolResult
		evidence bool
	}{
		{"bytes", func() *mcpsdk.CallToolResult {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("x", MaxResultBytes)}}}
		}, false},
		{"blocks", func() *mcpsdk.CallToolResult {
			r := &mcpsdk.CallToolResult{Content: make([]mcpsdk.Content, MaxContentBlocks+1)}
			for i := range r.Content {
				r.Content[i] = &mcpsdk.TextContent{Text: "x"}
			}
			return r
		}, true},
		{"sampling-only", func() *mcpsdk.CallToolResult {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.ToolUseContent{Name: "nested", ID: "nested", Input: map[string]any{}}}}
		}, true},
		{"empty audio", func() *mcpsdk.CallToolResult {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.AudioContent{MIMEType: "audio/wav"}}}
		}, true},
		{"needs input", func() *mcpsdk.CallToolResult {
			r := &mcpsdk.CallToolResult{}
			_ = json.Unmarshal([]byte(`{"content":[],"resultType":"input_required","inputRequests":{}}`), r)
			return r
		}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := fixtureServer("2026-07-28")
			var calls atomic.Int32
			addTool(server, &mcpsdk.Tool{Name: "act", InputSchema: json.RawMessage(`{"type":"object"}`)})
			server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
				return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
					if method == "tools/call" {
						calls.Add(1)
						return test.reply(), nil
					}
					return next(ctx, method, req)
				}
			})
			c := connectMemory(t, server)
			call, err := c.PrepareCall(t.Context(), callRef(t, c, "act"), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			result, err := call.Invoke(t.Context())
			errorKind(t, err, "operation_unknown")
			if test.evidence != (len(result.Result) > 0) || calls.Load() != 1 {
				t.Fatal("evidence/retry", len(result.Result), calls.Load())
			}
			_, err = call.Invoke(t.Context())
			errorKind(t, err, "operation_conflict")
		})
	}
}
