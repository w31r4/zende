package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func TestRequestTimeout(t *testing.T) {
	for _, method := range []string{"resource.register", "resource.refresh", "mcp.search"} {
		if got := requestTimeout(method, struct{}{}); got != mcp.RefreshTimeout+10*time.Second {
			t.Fatal(method, got)
		}
	}
	if got := requestTimeout("resource.status", struct{}{}); got != 10*time.Second {
		t.Fatal(got)
	}
	if got := requestTimeout("task.wait", protocol.WaitTask{WaitMS: 30000}); got != 35*time.Second {
		t.Fatal(got)
	}
}

// Executed only by the installed fixture launcher, over real stdio MCP.
func TestInstalledMCPHelper(t *testing.T) {
	if os.Getenv("ZENDE_INSTALLED_MCP_HELPER") != "1" {
		return
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "installed-fixture", Version: "1"}, &mcpsdk.ServerOptions{
		SupportedProtocolVersions: []string{"2025-11-25"},
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "initialize" {
				// Exceeds the old CLI deadline, but fits the MCP preparation budget.
				timer := time.NewTimer(11 * time.Second)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
				}
			}
			return next(ctx, method, req)
		}
	})
	server.AddTool(&mcpsdk.Tool{Name: "record", Description: "private-installed-schema-marker", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var input struct{ Text string }
			if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
				return nil, err
			}
			file, err := os.OpenFile("mcp-effects", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			_, writeErr := fmt.Fprintln(file, input.Text)
			closeErr := file.Close()
			if writeErr != nil {
				return nil, writeErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "recorded:" + input.Text}}}, nil
		})
	err := server.Run(context.Background(), &mcpsdk.StdioTransport{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0) // Never write the Go test runner's PASS banner to MCP stdout.
}

// V09: deterministic model, real shell/CLI/UDS and installed stdio server.
// The core and process manager live in this test process, not deployed services.
func TestModelInstallsRepairsAndUsesMCPThroughBinaryCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-mcp-install-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	t.Setenv("ZENDE_MCP_INSTALL_FIXTURE", "1")
	paths, err := (&app{profile: "mcp-install"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manager, err := process.NewManager(filepath.Join(paths.State, "processes"), "/bin/sh", 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	c.Runner = manager
	c.Tools, err = tool.NewRegistry(append(tool.ProcessExecutors(manager), c.MCPExecutors()...)...)
	if err != nil {
		t.Fatal(err)
	}
	c.Commands = resourceHints(binary, "mcp-install")
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	doneServer := make(chan error, 1)
	go func() { doneServer <- server.Serve(ctx) }()
	defer func() { cancel(); <-doneServer }()
	p := protocol.CreateAgent{Cwd: dir, Input: "Install an MCP server and use it in this task; repair a failed update"}
	req, _ := protocol.NewRequest("agent.create", "mcp-install-task", p)
	a, err := s.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// A failed test may leave an accepted shell alive after observation stops.
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, err := manager.FreezeStop(cleanup, "fixture-cleanup", process.StopScope{Agents: []string{a.AgentID}})
		if err != nil {
			t.Error(err)
			return
		}
		for cleanup.Err() == nil {
			record, err := manager.AdvanceStop(cleanup, "fixture-cleanup")
			if err != nil {
				t.Error(err)
				return
			}
			if record.State == "stopped" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fixture processes did not stop", cleanup.Err())
	}()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nprintf x >> mcp-starts\nexec " + quoteCommandPart(testBinary) + " '-test.run=^TestInstalledMCPHelper$'\n"
	config, _ := json.Marshal(resource.MCPConfig{SchemaVersion: 1, Name: "installed-fixture", Transport: "stdio",
		Command: filepath.Join(dir, "installed", "server"), Cwd: dir,
		Env: map[string]string{"ZENDE_INSTALLED_MCP_HELPER": "env:ZENDE_MCP_INSTALL_FIXTURE"}})
	cli := quoteCommandPart(binary) + " --profile mcp-install "
	agent := quoteCommandPart(a.AgentID)
	var resourceID string
	var ready protocol.ResourceInfo
	var reference, catalog, system string
	var commandID string
	var stdout, stderr strings.Builder
	offsets := map[string]int64{}
	stage, calls, waits := 0, 0, 0
	emitCall := func(name string, args any) []model.Delta {
		calls++
		raw, _ := json.Marshal(args)
		return fixture.Call(fmt.Sprintf("call-%d", calls), name, string(raw))
	}
	command := func(text string) []model.Delta {
		commandID = ""
		stdout.Reset()
		stderr.Reset()
		offsets = map[string]int64{}
		return emitCall("exec_command", map[string]any{"command": text, "wait_ms": 1000})
	}
	adapter := resourceModelFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) (resultErr error) {
		defer func() {
			if resultErr != nil {
				t.Logf("fixture stage %d: %v", stage, resultErr)
			}
		}()
		if system == "" {
			system = r.System
		}
		if system != r.System || strings.Contains(r.System, "private-installed-schema-marker") {
			return fmt.Errorf("system prompt changed or expanded an MCP definition")
		}
		if len(r.Tools) != 4 {
			return fmt.Errorf("native tool surface changed: %d", len(r.Tools))
		}
		for _, native := range r.Tools {
			if native.Name == "record" {
				return fmt.Errorf("server tool was eagerly registered")
			}
		}
		notices := 0
		for _, message := range r.Messages {
			for _, block := range message.Content {
				if strings.HasPrefix(block.Text, "<resource_changes ") {
					notices++
					if strings.Contains(block.Text, "private-installed-schema-marker") {
						return fmt.Errorf("resource notice expanded the server schema")
					}
				}
			}
		}
		if notices > 1 || stage < 3 && notices != 0 || stage >= 4 && notices != 1 {
			return fmt.Errorf("missing or duplicated ready notice at stage %d: %d", stage, notices)
		}
		var body string
		var exitCode int
		if stage > 0 {
			var last model.Message
			for i := len(r.Messages) - 1; i >= 0; i-- {
				if r.Messages[i].Role == "tool" {
					last = r.Messages[i]
					break
				}
			}
			if last.Role != "tool" || len(last.Content) != 1 || last.Content[0].IsError {
				return fmt.Errorf("missing successful host-tool result at stage %d: %+v", stage, last)
			}
			body = last.Content[0].Text
			if stage != 4 && stage != 5 && stage != 8 && stage != 9 {
				var result tool.ProcessResult
				if err := json.Unmarshal([]byte(body), &result); err != nil || result.ID == "" {
					return fmt.Errorf("missing process result at stage %d: %s", stage, body)
				}
				if commandID != "" && commandID != result.ID {
					return fmt.Errorf("waiting replaced the original CLI process")
				}
				commandID = result.ID
				for _, page := range result.Output {
					if page.Gap != nil {
						return fmt.Errorf("lost CLI output: %+v", page)
					}
					offsets[page.Stream] = page.Next
					if page.Stream == "stdout" {
						stdout.WriteString(page.Text)
					} else {
						stderr.WriteString(page.Text)
					}
				}
				if result.Status == "running" {
					waits++
					for _, event := range emitCall("write_stdin", map[string]any{"process_id": result.ID, "offsets": offsets, "wait_ms": 3000}) {
						if err := emit(event); err != nil {
							return err
						}
					}
					return nil
				}
				if result.Status != "exited" || result.ExitCode == nil {
					return fmt.Errorf("CLI did not exit: %+v", result)
				}
				exitCode, body = *result.ExitCode, stdout.String()
				if stage != 1 && stage != 6 && (exitCode != 0 || stderr.Len() != 0) {
					return fmt.Errorf("CLI failed at stage %d: %s", stage, stderr.String())
				}
			}
		}
		var events []model.Delta
		switch stage {
		case 0:
			events = command("printf %s " + quoteCommandPart(string(config)) + " > mcp.json && " + cli + "--request-id missing-server resource register " + agent + " mcp.json --kind mcp")
		case 1:
			var failure protocol.Error
			if exitCode == 0 || body != "" || json.Unmarshal([]byte(stderr.String()), &failure) != nil || failure.Data.Kind != "unavailable" || failure.Data.ResourceID == "" {
				return fmt.Errorf("missing dependency falsely succeeded: %d %q %q", exitCode, body, stderr.String())
			}
			resourceID = failure.Data.ResourceID
			events = command(cli + "resource status " + agent + " " + resourceID)
		case 2:
			var status protocol.ResourceStatus
			if json.Unmarshal([]byte(body), &status) != nil || status.State == "ready" || status.Version != "" || status.Update == nil || status.Update.State != "failed" {
				return fmt.Errorf("failed install published a ready resource: %s", body)
			}
			events = command("mkdir -p installed && printf %s " + quoteCommandPart(launcher) + " > installed/server && chmod +x installed/server && " + cli + "--request-id repaired-server resource refresh " + agent + " " + resourceID)
		case 3:
			var receipt protocol.Accepted
			if json.Unmarshal([]byte(body), &receipt) != nil || receipt.Resource == nil || receipt.Resource.ID != resourceID || receipt.Resource.Version == "" {
				return fmt.Errorf("installation did not publish ready: %s", body)
			}
			ready = *receipt.Resource
			events = emitCall("tool_search", map[string]any{"server_id": resourceID, "query": "record"})
		case 4, 8:
			var result mcp.SearchResult
			if json.Unmarshal([]byte(body), &result) != nil || len(result.Tools) != 1 || result.ResourceVersion != ready.Version || !strings.Contains(string(result.Tools[0].Tool), "private-installed-schema-marker") {
				return fmt.Errorf("complete definition not discovered: %s", body)
			}
			if stage == 8 && (reference != result.Tools[0].Ref || catalog != result.Version) {
				return fmt.Errorf("failed update replaced the ready catalog")
			}
			reference, catalog = result.Tools[0].Ref, result.Version
			events = emitCall("tool_invoke", protocol.MCPInvoke{ServerID: resourceID, ToolRef: reference, Arguments: json.RawMessage(fmt.Sprintf(`{"text":"stage-%d"}`, stage))})
		case 5, 9:
			var receipt protocol.MCPCallReceipt
			if json.Unmarshal([]byte(body), &receipt) != nil || receipt.State != "completed" || receipt.TaskID != a.TaskID || receipt.ResourceVersion != ready.Version || receipt.Result == nil {
				return fmt.Errorf("invocation missing original Task/version: %s", body)
			}
			page, err := s.ReadMCPResult(ctx, protocol.MCPResultRead{AgentID: a.AgentID, OperationID: receipt.OperationID, Part: "0"})
			if err != nil || page.Text != fmt.Sprintf("recorded:stage-%d", stage-1) {
				return fmt.Errorf("call result missing: %+v %v", page, err)
			}
			if stage == 9 {
				events = fixture.Answer("final", "installed and used; failed update kept the ready version")
			} else {
				events = command("printf %s broken > mcp.json && " + cli + "--request-id broken-update resource refresh " + agent + " " + resourceID)
			}
		case 6:
			var failure protocol.Error
			if exitCode == 0 || body != "" || json.Unmarshal([]byte(stderr.String()), &failure) != nil || failure.Data.Kind != "invalid_resource" {
				return fmt.Errorf("invalid update falsely succeeded: %d %q %q", exitCode, body, stderr.String())
			}
			events = command(cli + "resource status " + agent + " " + resourceID)
		case 7:
			var status protocol.ResourceStatus
			if json.Unmarshal([]byte(body), &status) != nil || status.State != "ready" || status.Version != ready.Version || status.Update == nil || status.Update.State != "failed" {
				return fmt.Errorf("failed update lost the previous version: %s", body)
			}
			events = emitCall("tool_search", map[string]any{"server_id": resourceID, "query": "record"})
		default:
			return fmt.Errorf("unexpected model continuation")
		}
		stage++
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", NativeCategories: true}, nil
	})
	doneCore := make(chan error, 1)
	go func() { doneCore <- c.Run(ctx) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			if err := <-doneCore; err != nil {
				t.Error(err)
			}
		}
	}()
	for {
		snap, err := s.Snapshot(ctx, a.AgentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Tasks) != 1 || snap.Tasks[0].ID != a.TaskID || snap.Agent.Cwd != dir {
			t.Fatal("installation replaced the original task", snap)
		}
		if snap.Tasks[0].State == "completed" {
			break
		}
		if snap.Tasks[0].State == "failed" || snap.Tasks[0].State == "unknown" || ctx.Err() != nil {
			t.Fatal(snap, ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Join before reading adapter-owned counters; keep the store open.
	cancel()
	err = <-doneCore
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	if waits == 0 || stage != 10 {
		t.Fatal("slow installation was not observed through the original process", waits, stage)
	}
	for name, want := range map[string]string{"mcp-starts": "x", "mcp-effects": "stage-4\nstage-8\n"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatal(name, string(data), err)
		}
	}
	history, err := s.Events(t.Context(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range history.Events {
		counts[event.Kind]++
		if event.Kind == "model.started" {
			var started struct {
				Catalog protocol.ResourceCatalog `json:"resource_catalog"`
			}
			if json.Unmarshal(event.Payload, &started) != nil || !started.Catalog.Valid() {
				t.Fatal("missing resource provenance", string(event.Payload))
			}
		}
	}
	if counts["task.accepted"] != 1 || counts["resource.published"] != 1 || counts["mcp.completed"] != 2 || counts["tool.dispatching"] != calls || counts["model.started"] != calls+1 {
		t.Fatal(counts, calls)
	}
}
