package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

// The full executable, SQLite, UDS, runner and shells are real. Model output is
// a local HTTP fixture; this is not real-provider or user-deployment acceptance.
func TestBinaryModelSpawnsThroughPipeAndPTYCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	binary := filepath.Join(t.TempDir(), "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%t", pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("tmux required")
				}
			}
			binarySpawn(t, binary, pty)
		})
	}
}

func binarySpawn(t *testing.T, binary string, pty bool) {
	dir := t.TempDir()
	other := filepath.Join(dir, "temporary-shell-cwd")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-spawn-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	var mu sync.Mutex
	counts := map[string]int{}
	frame := func(kind string, value map[string]any) string {
		value["type"] = kind
		b, _ := json.Marshal(value)
		return "data: " + string(b) + "\n\n"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions string           `json:"instructions"`
			Input        []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_, rest, _ := strings.Cut(request.Instructions, "\nAgent: ")
		agent := strings.SplitN(rest, "\n", 2)[0]
		if agent == "" || !strings.Contains(request.Instructions, "agent spawn --task-file") || !strings.Contains(request.Instructions, "agent wait") {
			t.Error("invalid caller/capabilities")
			return
		}
		mu.Lock()
		counts[agent]++
		n := counts[agent]
		mu.Unlock()
		// The most recent user input identifies this fixture's Task, independently
		// of inherited parent input and asynchronously scheduled requests.
		input := ""
		for _, item := range request.Input {
			if item["role"] == "user" {
				raw, _ := json.Marshal(item["content"])
				input = string(raw)
			}
		}
		child := strings.Contains(input, "child-work") || strings.Contains(input, "follow-up")
		var item map[string]any
		if !child && n == 1 {
			spawn := quoteCommandPart(binary) + " --profile test agent spawn child-work"
			command := "cd " + quoteCommandPart(other) + " && " + spawn + " && " + spawn
			args, _ := json.Marshal(map[string]any{"command": command, "pty": pty, "wait_ms": 5000})
			item = map[string]any{"type": "function_call", "id": "spawn-item", "call_id": "spawn-call", "name": "exec_command", "arguments": string(args), "status": "completed"}
		} else {
			answer := "parent done"
			if child {
				answer = "child done"
				if n == 1 {
					if len(request.Input) != 4 || request.Input[3]["role"] != "developer" {
						t.Error("inherited view or child input changed", request.Input)
					}
					for _, value := range request.Input {
						if value["type"] == "function_call" || value["type"] == "function_call_output" {
							t.Error("child inherited pending spawn call")
						}
					}
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
				} else if n != 2 || !strings.Contains(input, "follow-up") {
					t.Error("unexpected child request", n)
				}
			} else if n == 2 {
				if n != 2 || len(request.Input) < 3 {
					t.Error("unexpected parent continuation", n, request.Input)
					return
				}
				last := request.Input[len(request.Input)-1]
				output, _ := last["output"].(string)
				var result tool.ProcessResult
				if last["type"] != "function_call_output" || json.Unmarshal([]byte(output), &result) != nil || result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 {
					t.Error("spawn CLI did not exit", output)
					return
				}
				receipts := []protocol.Accepted{}
				for _, page := range result.Output {
					if page.Stream != "stdout" && page.Stream != "pty" {
						continue
					}
					for _, line := range strings.Split(strings.TrimSpace(page.Text), "\n") {
						// The model-facing projection escapes PTY CR as literal \x0d.
						// Raw bytes remain available through process output.
						line = strings.TrimSuffix(line, `\x0d`)
						var accepted protocol.Accepted
						if json.Unmarshal([]byte(line), &accepted) != nil {
							t.Error("CLI emitted non-JSON", line)
							return
						}
						receipts = append(receipts, accepted)
					}
				}
				if len(receipts) != 2 || receipts[0].AgentID != receipts[1].AgentID || receipts[0].TaskID != receipts[1].TaskID {
					t.Error("spawn retry created another identity", receipts)
					return
				}
			} else if n > 4 || !strings.Contains(input, "peer output") || !strings.Contains(input, "child done") {
				t.Error("invalid automatic result continuation", n, input)
				return
			}
			item = map[string]any{"type": "message", "id": fmt.Sprintf("answer-%d", n), "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\n" + answer + "\n</final>", "annotations": []any{}}}}
		}
		start := map[string]any{}
		for k, v := range item {
			start[k] = v
		}
		start["status"] = "in_progress"
		if item["type"] == "function_call" {
			start["arguments"] = ""
		} else {
			start["content"] = []any{}
		}
		response := fmt.Sprintf("%s-%d", agent, n)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frame("response.created", map[string]any{"response": map[string]any{"id": response, "status": "in_progress"}})+
			frame("response.output_item.added", map[string]any{"output_index": 0, "item": start})+
			frame("response.output_item.done", map[string]any{"output_index": 0, "item": item})+
			frame("response.completed", map[string]any{"response": map[string]any{"id": response, "status": "completed", "output": []any{item}}}))
	}))
	defer server.Close()
	configDir := filepath.Join(dir, "config", "zende", "test")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model: local/fixture\nconnections:\n  local:\n    protocol: responses\n    base_url: %s\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_") && !strings.HasPrefix(entry, "ZENDE_") {
			env = append(env, entry)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, service := range []string{"runner", "core"} {
		command := exec.CommandContext(ctx, binary, "--profile", "test", service, "serve")
		command.Env, command.Dir = env, dir
		var log bytes.Buffer
		command.Stdout, command.Stderr = &log, &log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		defer func() {
			release()
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Error(service, err, log.String())
				}
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error(service, "stop timeout")
			}
		}()
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Env, cmd.Dir = env, dir
		return cmd.CombinedOutput()
	}
	must := func(target any, args ...string) {
		t.Helper()
		out, err := run(args...)
		if err != nil || bytes.Contains(out, []byte{27}) {
			t.Fatal(args, err, string(out))
		}
		if err := json.Unmarshal(out, target); err != nil {
			t.Fatal(err, string(out))
		}
	}
	for _, service := range []string{"runner", "core"} {
		for deadline := time.Now().Add(5 * time.Second); ; {
			if _, err := run(service, "status"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(service, "not ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var parent protocol.Accepted
	must(&parent, "agent", "create", "parent-work", "--cwd", dir)
	waitTask := func(agent, task, state string) protocol.Snapshot {
		t.Helper()
		for deadline := time.Now().Add(8 * time.Second); ; {
			var snap protocol.Snapshot
			must(&snap, "agent", "inspect", agent)
			for _, saved := range snap.Tasks {
				if saved.ID == task && saved.State == state {
					return snap
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("task did not reach "+state, snap)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitTask(parent.AgentID, parent.TaskID, "completed")
	var agents []protocol.Agent
	must(&agents, "agent", "list")
	if len(agents) != 2 {
		t.Fatal("duplicated children", agents)
	}
	child := agents[1]
	var snap protocol.Snapshot
	must(&snap, "agent", "inspect", child.ID)
	if child.ParentID != parent.AgentID || child.Cwd != dir || len(snap.Tasks) != 1 || snap.Tasks[0].State != "running" {
		t.Fatal("spawn waited or followed temporary shell cwd", snap)
	}
	var history protocol.EventPage
	must(&history, "agent", "history", child.ID, "--limit", "1000")
	var source protocol.SpawnSource
	for _, event := range history.Events {
		if event.Kind == "context.inherited" {
			if err := json.Unmarshal(event.Payload, &source); err != nil {
				t.Fatal(err)
			}
		}
	}
	if source.Caller == nil || source.Caller.AgentID != parent.AgentID || source.Caller.TaskID != parent.TaskID || source.CallID != "spawn-call" || source.Messages != 1 || source.ExcludedPending != 1 {
		t.Fatal(source)
	}
	var state process.State
	must(&state, "process", "inspect", source.Caller.OperationID)
	if state.PTY != pty || state.AgentID != parent.AgentID || state.TaskID != parent.TaskID {
		t.Fatal(state)
	}
	if pty {
		if state.Terminal == nil {
			t.Fatal("missing test terminal")
		}
		defer func() { _, _ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").CombinedOutput() }()
	}
	release()
	waitTask(child.ID, snap.Tasks[0].ID, "completed")
	var next protocol.Accepted
	must(&next, "--request-id", "reuse", "agent", "submit", child.ID, "follow-up")
	final := waitTask(child.ID, next.TaskID, "completed")
	if len(final.Tasks) != 2 || final.Tasks[0].ID != snap.Tasks[0].ID || final.Tasks[1].Input != "follow-up" {
		t.Fatal("reuse replaced Agent history", final)
	}
	for deadline := time.Now().Add(8 * time.Second); ; {
		var mail protocol.MailboxPage
		must(&mail, "mailbox", "list", "--agent", parent.AgentID)
		if len(mail.Items) == 2 && mail.Items[0].State == "processed" && mail.Items[1].State == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic results not processed", mail)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[parent.AgentID] != 4 || counts[child.ID] != 2 || len(counts) != 2 {
		t.Fatal("unexpected loops", counts)
	}
}
