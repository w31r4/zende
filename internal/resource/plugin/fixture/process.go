// Package fixture provides a local subprocess for plugin integration tests.
// Production packages do not import it. Every path comes from the test process.
package fixture

import (
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

// RunFromEnv returns in the parent test and exits in its plugin subprocess.
func RunFromEnv() {
	if os.Getenv("ZENDE_TEST_PLUGIN") != "1" {
		return
	}
	audit := func(method string) {
		if path := os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(2)
			}
			err = json.NewEncoder(f).Encode(map[string]any{"method": method, "pid": os.Getpid()})
			f.Close()
			if err != nil {
				os.Exit(2)
			}
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), plugin.MaxFrameBytes+1)
	respond := func(id string, result any) {
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) != nil {
			os.Exit(7)
		}
	}
	progress := func(id, operation string, sequence int64) {
		p := plugin.Progress{RequestID: id, OperationID: operation, Sequence: sequence, Value: json.RawMessage(`{"text":"private-progress"}`)}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": p}) != nil {
			os.Exit(7)
		}
	}
	var waitingID, waitingOperation string
	for scanner.Scan() {
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(3)
		}
		audit(req.Method)
		var result any
		switch req.Method {
		case "plugin.initialize":
			var params plugin.InitializeParams
			if json.Unmarshal(req.Params, &params) != nil {
				os.Exit(4)
			}
			result = plugin.InitializeResult{ProtocolVersion: 1, Plugin: params.Plugin, Capabilities: params.Capabilities}
		case "plugin.describe":
			var query struct {
				Kinds []string `json:"kinds"`
			}
			if json.Unmarshal(req.Params, &query) != nil {
				os.Exit(4)
			}
			if path := os.Getenv("ZENDE_TEST_PLUGIN_REWRITE"); path != "" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					os.Exit(5)
				}
				_, err = f.WriteString("\n")
				f.Close()
				if err != nil {
					os.Exit(5)
				}
			}
			mode := os.Getenv("ZENDE_TEST_PLUGIN_MODE")
			if mode == "stall" {
				time.Sleep(time.Minute)
			}
			input := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"additionalProperties":false}`)
			if mode == "invalid" {
				input = json.RawMessage(`{"type":"array"}`)
			}
			callable := plugin.Callable{Name: "echo", Description: "Local fixture", InputSchema: input, OutputSchema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`)}
			catalog := map[string]any{}
			for _, kind := range []string{"tools", "commands"} {
				if slices.Contains(query.Kinds, kind) {
					catalog[kind] = []plugin.Callable{callable}
				}
			}
			if slices.Contains(query.Kinds, "providers") {
				model := protocol.ModelInfo{ID: "fixture-text", ContextWindow: 32000, MaxOutput: 4096, Efforts: []string{"low", "high"}, NativeCategories: true}
				if mode == "invalid-provider" {
					model.ContextWindow = 0
				}
				catalog["providers"] = []plugin.Provider{{Name: "fixture-models", Description: "Local model fixture", Models: []protocol.ModelInfo{model}}}
			}
			if slices.Contains(query.Kinds, "resources") {
				files := []protocol.ResourceFileInfo{{Path: "instructions.md", Bytes: 100, SHA256: strings.Repeat("0", 64)}}
				if mode == "invalid-resource" {
					files[0].Path = "../outside"
				}
				catalog["resources"] = []plugin.Resource{{Name: "fixture-prompt", Description: "Local instruction fixture", Kind: "prompt", Entry: "instructions.md", Files: files}}
			}
			result = catalog
		case "plugin.tool.call", "plugin.command.call":
			var p struct {
				OperationID string          `json:"operation_id"`
				Arguments   json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				os.Exit(4)
			}
			progress(req.ID, p.OperationID, 1)
			switch os.Getenv("ZENDE_TEST_PLUGIN_MODE") {
			case "call-block":
				deadline := time.Now().Add(15 * time.Second)
				for {
					if _, err := os.Stat(os.Getenv("ZENDE_TEST_PLUGIN_RELEASE")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						os.Exit(9)
					}
					time.Sleep(10 * time.Millisecond)
				}
				result = plugin.Completion{OperationID: p.OperationID, State: "completed", Output: p.Arguments}
			case "call-exit":
				os.Exit(8)
			case "call-cancel":
				waitingID, waitingOperation = req.ID, p.OperationID
				continue
			case "call-failed":
				result = plugin.Completion{OperationID: p.OperationID, State: "failed", Error: &plugin.CallFailure{Kind: "fixture_failed", Message: "private-failure"}}
			case "call-failed-large":
				result = plugin.Completion{OperationID: p.OperationID, State: "failed", Output: p.Arguments, Error: &plugin.CallFailure{Kind: "fixture_failed", Message: strings.Repeat("<", 8192)}}
			default:
				result = plugin.Completion{OperationID: p.OperationID, State: "completed", Output: p.Arguments}
			}
		case "plugin.cancel":
			if waitingID == "" {
				os.Exit(6)
			}
			progress(waitingID, waitingOperation, 2)
			respond(waitingID, plugin.Completion{OperationID: waitingOperation, State: "cancelled"})
			waitingID, waitingOperation = "", ""
			continue
		case "plugin.shutdown":
		default:
			os.Exit(6)
		}
		respond(req.ID, result)
		if req.Method == "plugin.shutdown" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}
