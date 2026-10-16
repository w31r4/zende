package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

// Uses the deployed CLI and Core RPC with a real independent fixture process.
// Caller events are seeded here; this is not a live-model integration test.
func checkPluginCLICommands(t *testing.T, ctx context.Context, s *store.Store, binary, source string, run func(bool, ...string) []byte) {
	t.Helper()
	for _, tc := range []struct {
		mode, state string
		caller      bool
	}{{"", "completed", true}, {"call-failed", "failed", false}, {"call-cancel", "cancelled", true}, {"call-exit", "unknown", false}} {
		func() {
			before, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(protocol.CallerEnvironment, "")
			t.Setenv("ZENDE_TEST_PLUGIN_MODE", tc.mode)
			var agent protocol.Accepted
			if err := json.Unmarshal(run(true, "agent", "create", "--cwd", filepath.Dir(source)), &agent); err != nil {
				t.Fatal(err)
			}
			var publication protocol.Accepted
			if err := json.Unmarshal(run(true, "resource", "register", agent.AgentID, source, "--kind", "plugin"), &publication); err != nil {
				t.Fatal(err)
			}
			var catalog resource.PluginCatalog
			if err := json.Unmarshal(run(true, "plugin", "catalog", agent.AgentID, publication.Resource.ID), &catalog); err != nil {
				t.Fatal(err)
			}
			var ref, toolRef string
			for _, d := range catalog.Definitions {
				if d.Kind == "command" {
					ref = d.Ref
				} else if d.Kind == "tool" {
					toolRef = d.Ref
				}
			}
			var caller *protocol.Caller
			if tc.caller {
				p := protocol.Submit{AgentID: agent.AgentID, Input: "call plugin"}
				req, _ := protocol.NewRequest("agent.submit", "task-"+tc.state, p)
				a, err := s.Submit(ctx, req, p, 8)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.ClaimNext(ctx, agent.AgentID); err != nil {
					t.Fatal(err)
				}
				caller = &protocol.Caller{AgentID: agent.AgentID, TaskID: a.TaskID, OperationID: "exec-" + tc.state}
				if err := s.AppendTaskEvent(ctx, agent.AgentID, a.TaskID, "model.started", map[string]any{"request_id": "model", "settings": map[string]any{"cwd": filepath.Dir(source)}}); err != nil {
					t.Fatal(err)
				}
				if err := s.AppendTaskEvent(ctx, agent.AgentID, a.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "model", "call": model.ToolCall{ID: "shell", Name: "exec_command"}}); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(caller)
				t.Setenv(protocol.CallerEnvironment, string(raw))
			}
			argumentFile := filepath.Join(t.TempDir(), "中文 arguments.json")
			arguments := `{"text":"private-command-value\u001b[31m中文"}`
			if err := os.WriteFile(argumentFile, []byte(arguments), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"plugin", "command", agent.AgentID, publication.Resource.ID, ref, "--file", argumentFile}
			var receipt store.PluginReceipt
			if err := json.Unmarshal(run(true, args...), &receipt); err != nil || receipt.Kind != "command" || receipt.State != "dispatched" {
				t.Fatal(receipt, err)
			}
			if (receipt.Source == nil) != (caller == nil) || caller != nil && (receipt.Source.Caller != *caller || receipt.TaskID != caller.TaskID) {
				t.Fatal(receipt)
			}
			if caller != nil {
				var replay store.PluginReceipt
				if err := json.Unmarshal(run(true, args...), &replay); err != nil || replay.OperationID != receipt.OperationID || replay.Binding != receipt.Binding {
					t.Fatal(replay, err)
				}
			}
			if tc.state == "cancelled" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					r, err := s.PluginCall(ctx, receipt.PluginCallTarget)
					if err != nil {
						t.Fatal(err)
					}
					if r.Receipt.ProgressSeq > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("no progress")
					}
					time.Sleep(5 * time.Millisecond)
				}
				cancelArgs := []string{"--request-id", "cancel-" + tc.state, "plugin", "cancel", agent.AgentID, receipt.OperationID}
				first := run(true, cancelArgs...)
				if !bytes.Equal(first, run(true, cancelArgs...)) {
					t.Fatal("cancel retry changed identity")
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var current store.PluginReceipt
				if err := json.Unmarshal(run(true, "plugin", "inspect", agent.AgentID, receipt.OperationID), &current); err != nil {
					t.Fatal(err)
				}
				if current.State == tc.state {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no terminal result", current)
				}
				time.Sleep(5 * time.Millisecond)
			}
			// New CLI, stdin instead of file, and no caller: retry the exact effect
			// after its terminal result. It must keep the original provenance.
			t.Setenv(protocol.CallerEnvironment, "")
			cmd := exec.CommandContext(ctx, binary, "--profile", "plugins", "--request-id", receipt.OperationID, "plugin", "command", agent.AgentID, publication.Resource.ID, ref, "--file", "-")
			cmd.Stdin = strings.NewReader(arguments)
			var out, diagnostic bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &diagnostic
			if err := cmd.Run(); err != nil || diagnostic.Len() != 0 {
				t.Fatal(err, diagnostic.String())
			}
			var replay store.PluginReceipt
			if err := json.Unmarshal(out.Bytes(), &replay); err != nil || replay.State != tc.state || replay.Binding != receipt.Binding || replay.TaskID != receipt.TaskID {
				t.Fatal(replay, err)
			}
			var page store.PluginResultPage
			if err := json.Unmarshal(run(true, "plugin", "result", agent.AgentID, receipt.OperationID), &page); err != nil || !json.Valid([]byte(page.Text)) {
				t.Fatal(page, err)
			}
			var result plugin.CallResult
			if err := json.Unmarshal([]byte(page.Text), &result); err != nil || result.State != tc.state {
				t.Fatal(result, err)
			}
			if tc.state == "completed" && (result.Completion == nil || !strings.Contains(string(result.Completion.Output), "private-command-value")) {
				t.Fatal(result)
			}
			for _, bad := range [][]string{
				{"plugin", "command", agent.AgentID, publication.Resource.ID, ref},
				{"plugin", "command", agent.AgentID, publication.Resource.ID, toolRef, "--file", argumentFile},
			} {
				run(false, bad...)
			}
			if err := os.WriteFile(argumentFile, []byte("invalid json"), 0600); err != nil {
				t.Fatal(err)
			}
			run(false, args...)
			if tc.state == "unknown" {
				if err := os.WriteFile(argumentFile, []byte(arguments), 0600); err != nil {
					t.Fatal(err)
				}
				var failure protocol.Error
				if err := json.Unmarshal(run(false, args...), &failure); err != nil || failure.Data.Kind != "operation_unknown" {
					t.Fatal(failure, err)
				}
			}
			after, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
			if err != nil {
				t.Fatal(err)
			}
			method := []byte(`"method":"plugin.command.call"`)
			if bytes.Count(after, method)-bytes.Count(before, method) != 1 {
				t.Fatal("CLI replay dispatched the command again", tc.state)
			}
		}()
	}
	t.Setenv(protocol.CallerEnvironment, "")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
}
