package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func nativeMCPServer(t *testing.T, c *Core, agent string, handler mcpsdk.ToolHandler) (string, string) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "1"}, &mcpsdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	server.AddTool(&mcpsdk.Tool{Name: "effect", Description: "private-definition-marker", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}, handler)
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	t.Cleanup(c.CloseResources)
	config, _ := json.Marshal(resource.MCPConfig{SchemaVersion: 1, Name: "fixture", Transport: "streamable-http", URL: httpServer.URL})
	source := filepath.Join(c.Config.Config.Cwd, "mcp.json")
	if err := os.WriteFile(source, config, 0600); err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("resource.register", protocol.NewID("reg"), protocol.RegisterResource{AgentID: agent, Kind: "mcp", Path: source})
	v, err := c.Handle(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	id := v.(protocol.Accepted).Resource.ID
	page, err := c.searchMCP(t.Context(), protocol.MCPSearch{AgentID: agent, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return id, page.Tools[0].Ref
}

func nativeMCPDispatch(t *testing.T, c *Core, a protocol.Accepted, appendEvent func(string, any), input protocol.MCPInvoke) (mcpTool, tool.Call) {
	t.Helper()
	raw, _ := json.Marshal(input)
	call := model.ToolCall{ID: "model-call", Name: "tool_invoke", Arguments: raw}
	journalResponse(t, c, a, call)
	op := invocation{SchemaVersion: 2, ID: "original-call", RequestID: "request-original", Call: call, Version: "1"}
	appendEvent("tool.started", op)
	appendEvent("tool.dispatching", op)
	return mcpTool{core: c, invoke: true}, tool.Call{ID: op.ID, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Arguments: raw}
}

func TestMCPNativeReceiptRecoveryAndParts(t *testing.T) {
	c, a, event := journalWork(t)
	var effects atomic.Int32
	long := strings.Repeat("你好🙂", 24000)
	id, ref := nativeMCPServer(t, c, a.AgentID, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		record, err := c.Store.MCPCall(ctx, protocol.MCPCallTarget{AgentID: a.AgentID, OperationID: "original-call"})
		if err != nil || record == nil || record.Receipt.State != "dispatched" {
			return nil, fmt.Errorf("effect preceded durable dispatch: %v", err)
		}
		effects.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: long}, &mcpsdk.ImageContent{Data: []byte{1, 2, 3, 4, 5}, MIMEType: "image/png"}, &mcpsdk.ResourceLink{Name: "unfetched", URI: "https://example.invalid/no-network", MIMEType: "text/plain"}}, StructuredContent: map[string]any{"answer": 42}}, nil
	})
	executor, call := nativeMCPDispatch(t, c, a, event, protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"work"}`)})
	result, err := executor.Execute(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || len(result.Content[0].Text) > 64<<10 {
		t.Fatal("unbounded native result", len(result.Content[0].Text))
	}
	var receipt protocol.MCPCallReceipt
	if err := json.Unmarshal([]byte(result.Content[0].Text), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "completed" || receipt.OperationID != call.ID || receipt.Result.ContentBlocks != 3 || receipt.Result.Parts[1].More != true {
		t.Fatal(receipt)
	}
	for range 2 {
		again, err := executor.Execute(t.Context(), call)
		if err != nil || !reflect.DeepEqual(result, again) {
			t.Fatal(again, err)
		}
	}
	if effects.Load() != 1 {
		t.Fatal("duplicate effect", effects.Load())
	}
	changed := call
	changed.Arguments = json.RawMessage(strings.Replace(string(call.Arguments), "work", "changed", 1))
	if _, err := executor.Execute(t.Context(), changed); !isFailure(err, "operation_conflict") {
		t.Fatal("conflicting retry", err)
	}
	var text strings.Builder
	for offset := 0; ; {
		p, err := c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: a.AgentID, OperationID: call.ID, Part: "0", Offset: offset, Limit: 4096})
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(p.Text)
		if !p.More {
			break
		}
		if p.Next <= offset {
			t.Fatal("no progress")
		}
		offset = p.Next
	}
	if text.String() != long {
		t.Fatal("text pages lost bytes")
	}
	page, err := c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: a.AgentID, OperationID: call.ID, Part: "1", Limit: 4})
	if err != nil || page.Encoding != "base64" || !reflect.DeepEqual(page.Data, []byte{1, 2, 3, 4}) || !page.More {
		t.Fatal(page, err)
	}
	page, err = c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: a.AgentID, OperationID: call.ID, Part: "2"})
	if err != nil || !strings.Contains(page.Text, "https://example.invalid/no-network") {
		t.Fatal(page, err)
	}
	if _, err := c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: "other", OperationID: call.ID}); !isFailure(err, "not_found") {
		t.Fatal("cross-Agent result", err)
	}
	// Crash after the MCP receipt but before tool.completed/context append.
	c.CloseResources()
	c.Store.Close()
	reopened, err := store.Open(filepath.Join(c.Config.Config.Cwd, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := New(reopened, c.Config)
	replacement.Tools, err = tool.NewRegistry(replacement.MCPExecutors()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.InterruptMCPCalls(t.Context(), replacement.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.HoldInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	j, err := readRecoveryJournal(t.Context(), reopened, a.AgentID, a.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := replacement.verifyCalls(t.Context(), j)
	if err != nil || len(verified) != 1 {
		t.Fatal(verified, err)
	}
	if err := reopened.RecordVerifiedResults(t.Context(), a.AgentID, a.TaskID, j.Work.Agent.Seq, verified); err != nil {
		t.Fatal(err)
	}
	journal, err := replacement.Recovery(t.Context(), protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: a.TaskID})
	if err != nil || journal.NextStep != "continue_model" || effects.Load() != 1 {
		t.Fatal(journal, err, effects.Load())
	}
	if len(replacement.mcpSlots) != 0 {
		t.Fatal("recovery reconnected a server")
	}
}

func TestMCPNativeInterruptedDispatchDoesNotReplay(t *testing.T) {
	for _, stage := range []string{"not_dispatched", "dispatched", "failed", "unknown"} {
		t.Run(stage, func(t *testing.T) {
			c, a, event := journalWork(t)
			var effects atomic.Int32
			id, ref := nativeMCPServer(t, c, a.AgentID, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				effects.Add(1)
				return &mcpsdk.CallToolResult{}, nil
			})
			executor, call := nativeMCPDispatch(t, c, a, event, protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"work"}`)})
			if stage != "not_dispatched" {
				s, err := c.readyMCP(t.Context(), protocol.ResourceTarget{AgentID: a.AgentID, ID: id})
				if err != nil {
					t.Fatal(err)
				}
				prepared, release, err := s.conn.LeaseCall(t.Context(), ref, []byte(`{"text":"work"}`))
				if err != nil {
					s.unlock()
					t.Fatal(err)
				}
				input, _ := mcpInvocation(call)
				input.Binding = protocol.MCPCallBinding{ResourceID: id, ResourceVersion: s.pkg.Info.Version, Generation: s.generation, CatalogVersion: prepared.CatalogVersion(), ToolRef: ref, Definition: prepared.Definition().Tool}
				_, fresh, err := c.Store.BeginMCPCall(t.Context(), input, c.InstanceID)
				s.unlock()
				release()
				if err != nil || !fresh {
					t.Fatal(fresh, err)
				}
				if stage == "failed" || stage == "unknown" {
					kind := "stale_reference"
					var evidence json.RawMessage
					if stage == "unknown" {
						kind = "operation_unknown"
						evidence = json.RawMessage(`{"content":[{"type":"text","text":"unverified evidence"}]}`)
					}
					if _, err := c.Store.FinishMCPCall(t.Context(), input, c.InstanceID, evidence, protocol.E(kind, "fixture")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := c.Store.InterruptMCPCalls(t.Context(), "replacement"); err != nil {
				t.Fatal(err)
			}
			c.CloseResources()
			recovered, err := executor.Recover(t.Context(), call)
			if stage == "dispatched" || stage == "unknown" {
				if !isFailure(err, "operation_unknown") {
					t.Fatal(recovered, err)
				}
			} else {
				if err != nil || recovered.Error == nil {
					t.Fatal(recovered, err)
				}
				want := "mcp_not_dispatched"
				if stage == "failed" {
					want = "stale_reference"
				}
				if recovered.Error.Data.Kind != want {
					t.Fatal(recovered)
				}
			}
			if effects.Load() != 0 {
				t.Fatal("recovery executed an effect")
			}
			if stage == "unknown" {
				page, err := c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: a.AgentID, OperationID: call.ID, Part: "0"})
				if err != nil || page.Text != "unverified evidence" {
					t.Fatal(page, err)
				}
			}
		})
	}
}

func TestMCPNativePreDispatchValidationAndResultBudget(t *testing.T) {
	c, a, _ := journalWork(t)
	var effects atomic.Int32
	id, ref := nativeMCPServer(t, c, a.AgentID, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		effects.Add(1)
		return &mcpsdk.CallToolResult{}, nil
	})
	executor := mcpTool{core: c, invoke: true}
	for _, test := range []struct{ ref, args, kind string }{
		{ref, `{}`, "invalid_arguments"}, {ref, `{"text":2}`, "invalid_arguments"}, {"old-ref", `{"text":"go"}`, "stale_reference"}, {ref, `{"text":"go"}`, "caller_unknown"},
	} {
		raw, _ := json.Marshal(protocol.MCPInvoke{ServerID: id, ToolRef: test.ref, Arguments: json.RawMessage(test.args)})
		call := tool.Call{ID: protocol.NewID("unrecorded"), AgentID: a.AgentID, TaskID: a.TaskID, Arguments: raw}
		if _, err := executor.Execute(t.Context(), call); !isFailure(err, test.kind) {
			t.Fatal(test, err)
		}
		if saved, err := c.Store.MCPCall(t.Context(), protocol.MCPCallTarget{AgentID: a.AgentID, OperationID: call.ID}); err != nil || saved != nil {
			t.Fatal(saved, err)
		}
	}
	if effects.Load() != 0 {
		t.Fatal("invalid request executed")
	}
	if _, err := mcpJSONResult(strings.Repeat("x", model.MaxBlockBytes)); !isFailure(err, "result_budget") {
		t.Fatal("unbounded search result", err)
	}
	prompt := BuildPrompt(PromptInput{Tools: []tool.Descriptor{mcpTool{core: c}.Descriptor(), executor.Descriptor()}})
	for _, want := range []string{"tool_search:", "tool_invoke:", "untrusted data", "unknown operation"} {
		if !strings.Contains(prompt, want) {
			t.Fatal("missing prompt guidance", want)
		}
	}
	if strings.Contains(prompt, "private-definition-marker") {
		t.Fatal("prompt expands tools")
	}
}

func TestMCPNativeLoopSearchInvokeFinal(t *testing.T) {
	var effects atomic.Int32
	c := loopCore(t, nil)
	a := createWork(t, c, "discover and invoke")
	id, ref := nativeMCPServer(t, c, a.AgentID, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		effects.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "done"}}}, nil
	})
	search, _ := json.Marshal(map[string]string{"server_id": id, "query": "effect"})
	invoke, _ := json.Marshal(protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"go"}`)})
	script := fixture.New(fixture.Step{Events: fixture.Call("search", "tool_search", string(search)), Check: func(r model.Request) error {
		if len(r.Tools) != 2 {
			return fmt.Errorf("expected two stable tools, got %d", len(r.Tools))
		}
		for _, d := range r.Tools {
			if strings.Contains(d.Description, "private-definition-marker") {
				return fmt.Errorf("server tools expanded")
			}
		}
		return nil
	}}, fixture.Step{Events: fixture.Call("invoke", "tool_invoke", string(invoke)), Check: func(r model.Request) error {
		if !strings.Contains(r.Messages[len(r.Messages)-1].Content[0].Text, ref) {
			return fmt.Errorf("missing discovered reference")
		}
		return nil
	}}, fixture.Step{Events: fixture.Answer("final", "complete"), Check: func(r model.Request) error {
		if !strings.Contains(r.Messages[len(r.Messages)-1].Content[0].Text, `"state":"completed"`) {
			return fmt.Errorf("missing durable receipt")
		}
		return nil
	}})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: script, Model: "test", Protocol: "fixture", NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	var err error
	c.Tools, err = tool.NewRegistry(c.MCPExecutors()...)
	if err != nil {
		t.Fatal(err)
	}
	runCore(t, c)
	waitTask(t, c, a, "completed")
	if effects.Load() != 1 || len(script.Requests()) != 3 {
		t.Fatal(effects.Load(), len(script.Requests()))
	}
}

func TestMCPNativeCoreRestartFinishesOriginalTask(t *testing.T) {
	for _, remoteError := range []bool{false, true} {
		t.Run(fmt.Sprint(remoteError), func(t *testing.T) {
			var effects atomic.Int32
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "recovered MCP task"), Check: func(r model.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if r.System != "original system prompt" || last.Role != "tool" || last.Content[0].CallID != "model-call" || last.Content[0].IsError != remoteError {
					return fmt.Errorf("original task context was not recovered")
				}
				return nil
			}})
			c := loopCore(t, script)
			var err error
			c.Tools, err = tool.NewRegistry(c.MCPExecutors()...)
			if err != nil {
				t.Fatal(err)
			}
			a, event, restorer := savedWork(t, c, script)
			c.RestoreModels = restorer
			id, ref := nativeMCPServer(t, c, a.AgentID, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				effects.Add(1)
				return &mcpsdk.CallToolResult{IsError: remoteError, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "original result"}}}, nil
			})
			executor, call := nativeMCPDispatch(t, c, a, event, protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"work"}`)})
			if _, err := executor.Execute(t.Context(), call); err != nil && (!remoteError || !isFailure(err, "mcp_tool_error")) {
				t.Fatal(err)
			}
			c.CloseResources()
			replacement := reopenCore(t, c)
			replacement.Tools, err = tool.NewRegistry(replacement.MCPExecutors()...)
			if err != nil {
				t.Fatal(err)
			}
			runCore(t, replacement)
			snap := waitTask(t, replacement, a, "completed")
			if len(snap.Tasks) != 1 || snap.Tasks[0].Result != "recovered MCP task" || effects.Load() != 1 || restorer.restores.Load() != 1 {
				t.Fatal(snap, effects.Load(), restorer.restores.Load())
			}
		})
	}
}

func TestMCPNativeCancellationRetainsUnknown(t *testing.T) {
	c, a, event := journalWork(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	id, ref := nativeMCPServer(t, c, a.AgentID, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-finish:
			return &mcpsdk.CallToolResult{}, nil
		}
	})
	defer close(finish)
	executor, call := nativeMCPDispatch(t, c, a, event, protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"work"}`)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := executor.Execute(ctx, call); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	cancel()
	select {
	case err := <-done:
		if !isFailure(err, "operation_unknown") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not settle")
	}
	record, err := c.Store.MCPCall(t.Context(), protocol.MCPCallTarget{AgentID: a.AgentID, OperationID: call.ID})
	if err != nil || record.Receipt.State != "unknown" {
		t.Fatal(record, err)
	}
}

func TestMCPNativeInFlightPublicationKeepsOriginalReceipt(t *testing.T) {
	c, a, event := journalWork(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	defer unblock()
	id, ref := nativeMCPServer(t, c, a.AgentID, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "old connection result"}}}, nil
	})
	executor, call := nativeMCPDispatch(t, c, a, event, protocol.MCPInvoke{ServerID: id, ToolRef: ref, Arguments: json.RawMessage(`{"text":"work"}`)})
	done := make(chan error, 1)
	go func() { _, err := executor.Execute(t.Context(), call); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	original, err := c.Store.MCPCall(t.Context(), protocol.MCPCallTarget{AgentID: a.AgentID, OperationID: call.ID})
	if err != nil {
		t.Fatal(err)
	}
	newID, newRef := nativeMCPServer(t, c, a.AgentID, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		t.Error("unexpected new dispatch")
		return &mcpsdk.CallToolResult{}, nil
	})
	if newID != id || newRef == ref {
		t.Fatal(newID, newRef)
	}
	stale := call
	stale.ID = "new-call-old-ref"
	if _, err := executor.Execute(t.Context(), stale); !isFailure(err, "stale_reference") {
		t.Fatal("old reference redirected", err)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old lease was not completed")
	}
	completed, err := c.Store.MCPCall(t.Context(), protocol.MCPCallTarget{AgentID: a.AgentID, OperationID: call.ID})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Receipt.State != "completed" || !reflect.DeepEqual(original.Invocation, completed.Invocation) {
		t.Fatal(completed)
	}
	if _, err := executor.Execute(t.Context(), call); err != nil {
		t.Fatal("original receipt lost after replacement", err)
	}
	page, err := c.Store.ReadMCPResult(t.Context(), protocol.MCPResultRead{AgentID: a.AgentID, OperationID: call.ID, Part: "0"})
	if err != nil || page.Text != "old connection result" {
		t.Fatal(page, err)
	}
}
