// Package fixture provides a local subprocess for plugin integration tests.
// Production packages do not import it. Every path comes from the test process.
package fixture

import (
	"bufio"
	"encoding/json"
	"os"
	"time"

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
			callable := plugin.Callable{Name: "echo", Description: "Local fixture", InputSchema: input}
			result = map[string]any{"tools": []plugin.Callable{callable}, "commands": []plugin.Callable{callable}}
		case "plugin.shutdown":
		default:
			os.Exit(6) // Discovery must never invoke a callable.
		}
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) != nil {
			os.Exit(7)
		}
		if req.Method == "plugin.shutdown" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}
