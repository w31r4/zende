// Package fixture provides a local subprocess for plugin integration tests.
// Production packages do not import it. Every path comes from the test process.
package fixture

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"zende.local/harness/internal/model"
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
	scanner.Buffer(make([]byte, 4096), plugin.MaxModelRequestBytes+1)
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
	var waitingModel bool
	var pluginVersion string
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
			if os.Getenv("ZENDE_TEST_PLUGIN_MODE") == "slow-initialize" {
				time.Sleep(11 * time.Second)
			}
			var params plugin.InitializeParams
			if json.Unmarshal(req.Params, &params) != nil {
				os.Exit(4)
			}
			result = plugin.InitializeResult{ProtocolVersion: 1, Plugin: params.Plugin, Capabilities: params.Capabilities}
			pluginVersion = params.Plugin.Version
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
				resources, _ := Resources(mode)
				if mode == "invalid-resource" {
					resources[0].Files[0].Path = "../outside"
				}
				catalog["resources"] = resources
			}
			result = catalog
		case "plugin.resource.read":
			var p plugin.ResourceFileRead
			if json.Unmarshal(req.Params, &p) != nil {
				os.Exit(4)
			}
			mode := os.Getenv("ZENDE_TEST_PLUGIN_MODE")
			if mode == "resource-block" {
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
			}
			_, files := Resources(mode)
			f, ok := files[p.Path]
			if !ok {
				os.Exit(6)
			}
			r := plugin.ResourceFileResult{Ref: p.Ref, ResourceFile: f}
			switch mode {
			case "resource-corrupt":
				r.Data = append(r.Data, '!')
			case "resource-wrong-ref":
				r.Ref = "wrong"
			case "resource-wrong-path":
				r.Path = "../outside"
			case "resource-executable":
				r.Executable = !r.Executable
			case "resource-missing":
				r.Data = nil
			}
			result = r
			if mode == "resource-null-executable" {
				result = map[string]any{"ref": r.Ref, "path": r.Path, "data": r.Data, "executable": nil}
			}
		case "plugin.model.stream":
			var p plugin.ModelParams
			if protocol.StrictDecode(req.Params, &p) != nil || p.OperationID != p.Request.ID || p.Request.Model != "fixture-text" || p.Name != "fixture-models" || p.Scope.AgentID == "" {
				os.Exit(4)
			}
			mode := os.Getenv("ZENDE_TEST_PLUGIN_MODE")
			var sequence int64
			emit := func(d model.Delta) {
				sequence++
				if json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "method": "plugin.model.delta", "params": plugin.ModelDelta{RequestID: req.ID, OperationID: p.OperationID, Sequence: sequence, Delta: d}}) != nil {
					os.Exit(7)
				}
			}
			results := 0
			for _, m := range p.Request.Messages {
				if m.Role == "tool" {
					results++
				}
			}
			if mode == "model-tools" && results < 2 || mode == "model-partial" {
				name, call := "echo", "first"
				if results == 1 {
					name, call = "fail", "second"
				}
				emit(model.Delta{Kind: model.CallStart, CallID: call, Name: name})
				emit(model.Delta{Kind: model.ArgumentsDelta, CallID: call, Text: `{"text":`})
				if mode == "model-partial" {
					os.Exit(8)
				}
				emit(model.Delta{Kind: model.ArgumentsDelta, CallID: call, Text: `"fixture"}`})
				emit(model.Delta{Kind: model.CallEnd, CallID: call})
			} else {
				emit(model.Delta{Kind: model.MessageStart, MessageID: "reply", Category: "final"})
				if mode == "model-cancel" {
					waitingID, waitingOperation, waitingModel = req.ID, p.OperationID, true
					continue
				}
				if mode == "model-block" {
					deadline := time.Now().Add(15 * time.Second)
					for {
						if _, err := os.Stat(os.Getenv("ZENDE_TEST_PLUGIN_RELEASE")); err == nil {
							break
						}
						if time.Now().After(deadline) {
							os.Exit(9)
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				text, _ := json.Marshal(map[string]any{"version": pluginVersion, "scope": p.Scope, "effort": p.Effort, "tool_results": results})
				emit(model.Delta{Kind: model.TextDelta, MessageID: "reply", Text: string(text)})
				emit(model.Delta{Kind: model.MessageEnd, MessageID: "reply"})
			}
			emit(model.Delta{Kind: model.ResponseEnd})
			result = plugin.ModelCompletion{OperationID: p.OperationID, State: "completed"}
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
			if waitingModel {
				respond(waitingID, plugin.ModelCompletion{OperationID: waitingOperation, State: "cancelled"})
			} else {
				progress(waitingID, waitingOperation, 2)
				respond(waitingID, plugin.Completion{OperationID: waitingOperation, State: "cancelled"})
			}
			waitingModel = false
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

// Resources is deterministic fixture data shared with package/store tests.
func Resources(mode string) ([]plugin.Resource, map[string]plugin.ResourceFile) {
	resources := []plugin.Resource{
		{Name: "fixture-prompt", Description: "Local instruction fixture", Kind: "prompt", Entry: "instructions.md"},
		{Name: "fixture-skill", Description: "Local skill fixture", Kind: "skill", Entry: "SKILL.md"},
	}
	files := map[string]plugin.ResourceFile{}
	for n := range resources {
		r := &resources[n]
		body := "Read this fixture; do not execute scripts automatically."
		if mode == "resource-updated" {
			body = "Updated fixture instructions."
		}
		description := r.Description
		if mode == "resource-frontmatter" {
			description = "Does not match the catalog"
		}
		entry := []byte(fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s\n", r.Name, description, body))
		bundle := []plugin.ResourceFile{{Path: r.Entry, Data: entry}}
		if r.Kind == "skill" {
			bundle = append(bundle, plugin.ResourceFile{Path: "scripts/run.sh", Data: []byte("#!/bin/sh\nexit 99\n"), Executable: true})
			if mode == "resource-large" {
				remaining := protocol.MaxResourceBytes - len(entry) - len(bundle[1].Data)
				for n := 0; remaining > 0; n++ {
					size := min(remaining, protocol.MaxResourceFileBytes)
					bundle = append(bundle, plugin.ResourceFile{Path: fmt.Sprintf("assets/%d", n), Data: make([]byte, size)})
					remaining -= size
				}
			}
		}
		for _, f := range bundle {
			r.Files = append(r.Files, protocol.ResourceFileInfo{Path: f.Path, Bytes: int64(len(f.Data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(f.Data)), Executable: f.Executable})
			files[f.Path] = f
		}
	}
	return resources, files
}
