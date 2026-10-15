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
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/tool"
)

// Exercise request refresh with real resource preparation and MCP connections.
// Installation is a fixture executor; binary/shell installation is tested in cli.
func TestMixedResourceBatchesKeepDiscoveryBoundedAndAppendOnly(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "batch-fixture", Version: "1"}, nil)
	for i := range 64 {
		server.AddTool(&mcpsdk.Tool{Name: fmt.Sprintf("tool-%02d", i), Description: "deferred-mcp-definition", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return nil, fmt.Errorf("discovery must not execute a tool")
			})
	}
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer httpServer.Close()
	script := fixture.New(
		fixture.Step{Events: fixture.Call("first-batch", "exec_command", `{"batch":1}`)},
		fixture.Step{Events: fixture.Call("second-batch", "exec_command", `{"batch":2}`)},
		fixture.Step{Events: fixture.Answer("final", "resources available")},
	)
	var c *Core
	executor := testTool{"exec_command", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		var input struct{ Batch int }
		if err := json.Unmarshal(call.Arguments, &input); err != nil {
			return tool.Result{}, err
		}
		register := func(kind, name string, content []byte) error {
			source := filepath.Join(call.Cwd, name)
			if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(source, content, 0600); err != nil {
				return err
			}
			req, _ := protocol.NewRequest("resource.register", protocol.NewID("batch"), protocol.RegisterResource{AgentID: call.AgentID, Kind: kind, Path: source})
			_, err := c.Handle(ctx, req)
			return err
		}
		first, last := 0, 9
		if input.Batch == 2 {
			first, last = 9, 10
		} else if input.Batch != 1 {
			return tool.Result{}, fmt.Errorf("unexpected batch")
		}
		for i := first; i < last; i++ {
			config, _ := json.Marshal(resource.MCPConfig{SchemaVersion: 1, Name: fmt.Sprintf("batch-%d", i), Transport: "streamable-http", URL: httpServer.URL})
			if err := register("mcp", fmt.Sprintf("mcp-%d.json", i), config); err != nil {
				return tool.Result{}, err
			}
		}
		for kind, name := range map[string]string{"skill": "review/SKILL.md", "prompt": "prompt.md"} {
			content := fmt.Sprintf("---\nname: review\ndescription: Review code\n---\ndeferred-instruction-body-%d\n", input.Batch)
			if err := register(kind, name, []byte(content)); err != nil {
				return tool.Result{}, err
			}
		}
		return tool.Result{Content: []model.Block{{Type: "text", Text: "batch published"}}}, nil
	}}
	c = loopCore(t, script, executor)
	defer c.CloseResources()
	var err error
	c.Tools, err = tool.NewRegistry(append([]tool.Executor{executor}, c.MCPExecutors()...)...)
	if err != nil {
		t.Fatal(err)
	}
	c.Commands = append(discoveryCommands(), CommandHint{Name: "mcp.search", Usage: "zende mcp search AGENT RESOURCE"})
	a := createWork(t, c, "install two batches without expanding every resource")
	before, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	runCore(t, c)
	snap := waitTask(t, c, a, "completed")
	requests := script.Requests()
	if len(requests) != 3 || len(snap.Tasks) != 1 || snap.Agent.Cwd != c.Config.Config.Cwd || snap.Agent.Model != "" {
		t.Fatal("publication changed task or selection", len(requests), snap)
	}
	for i, r := range requests {
		raw, err := json.Marshal(r)
		if err != nil || strings.Contains(string(raw), "deferred-mcp-definition") || strings.Contains(string(raw), "deferred-instruction-body") {
			t.Fatal("unsolicited resource contents entered the model request", err)
		}
		if !reflect.DeepEqual(r.Tools, requests[0].Tools) || len(r.Tools) != 3 || r.System != requests[0].System || r.Model != requests[0].Model {
			t.Fatal("publication rewrote the native tool surface or prompt")
		}
		if i > 0 && !reflect.DeepEqual(r.Messages[:len(requests[i-1].Messages)], requests[i-1].Messages) {
			t.Fatal("previous context prefix was modified")
		}
		changes := noticeChanges(t, r)
		if len(changes) != i {
			t.Fatal("missing or duplicated discovery notice", i, changes)
		}
		if i == 0 {
			continue
		}
		latest := changes[len(changes)-1]
		if i == 1 && (latest.Changed != 11 || latest.Available != 11 || !latest.More || len(latest.Items) != 8) {
			t.Fatal("first batch was not bounded", latest)
		}
		if i == 2 && (latest.Changed != 3 || latest.Available != 12 || latest.More || len(latest.Items) != 3 || latest.From != changes[0].Generation) {
			t.Fatal("second batch did not advertise only changes", latest)
		}
		kinds := map[string]bool{}
		for _, item := range latest.Items {
			kinds[item.Kind] = true
			if item.Source == "" || item.ID == "" || item.Version == "" {
				t.Fatal("notice lost resource provenance", item)
			}
		}
		if !kinds["mcp"] || !kinds["skill"] || !kinds["prompt"] {
			t.Fatal("notice omitted an installed resource kind", kinds)
		}
	}
	// Query the same published catalog used by resource list/status and mcp search.
	list, err := c.Store.Resources(t.Context(), protocol.ResourceList{AgentID: a.AgentID, Limit: 20})
	if err != nil || len(list.Items) != 12 {
		t.Fatal(list, err)
	}
	for _, info := range list.Items {
		status, err := c.resourceStatus(t.Context(), protocol.ResourceTarget{AgentID: a.AgentID, ID: info.ID})
		if err != nil || status.State != "ready" || status.Version != info.Version {
			t.Fatal(status, err)
		}
		if info.Kind == "mcp" {
			found, err := c.searchMCP(t.Context(), protocol.MCPSearch{AgentID: a.AgentID, ID: info.ID, Query: "tool-00", Limit: 1})
			if err != nil || len(found.Tools) != 1 || !strings.Contains(string(found.Tools[0].Tool), "deferred-mcp-definition") {
				t.Fatal("published MCP not discoverable on demand", found, err)
			}
		}
	}
	after, err := c.Store.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil || !reflect.DeepEqual(before.Events, after.Events[:len(before.Events)]) {
		t.Fatal("publication rewrote existing events", err)
	}
	catalogSizes := []int{}
	for _, event := range after.Events {
		if event.Kind == "model.started" {
			var record requestRecord
			if json.Unmarshal(event.Payload, &record) != nil || record.ResourceCatalog == nil || !record.ResourceCatalog.Valid() {
				t.Fatal("request did not retain resource versions")
			}
			catalogSizes = append(catalogSizes, len(record.ResourceCatalog.Entries))
		}
	}
	if !reflect.DeepEqual(catalogSizes, []int{0, 11, 12}) {
		t.Fatal(catalogSizes)
	}
}
