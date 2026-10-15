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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

// Real binary, runner and SQLite; the model is an isolated HTTP fixture.
func TestBinaryModelSelectsThroughCLIAtRequestBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary, nextDir := filepath.Join(dir, "zende"), filepath.Join(dir, "next 中文")
	if err := os.Mkdir(nextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-select-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	var requests atomic.Int32
	frame := func(kind string, fields map[string]any) string {
		fields["type"] = kind
		raw, _ := json.Marshal(fields)
		return "data: " + string(raw) + "\n\n"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions string           `json:"instructions"`
			Input        []map[string]any `json:"input"`
			Reasoning    struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		n := requests.Add(1)
		wantEffort := "low"
		if n == 1 {
			wantEffort = "high"
		}
		if request.Reasoning.Effort != wantEffort {
			t.Error("wrong request effort", n, request.Reasoning.Effort)
		}
		var item map[string]any
		switch n {
		case 1:
			if !strings.Contains(request.Instructions, "agent select AGENT") {
				t.Error("selection CLI missing from prompt")
			}
			_, identity, _ := strings.Cut(request.Instructions, "\nAgent: ")
			agent := strings.SplitN(identity, "\n", 2)[0]
			if agent == "" {
				t.Error("missing agent identity")
				return
			}
			command := quoteCommandPart(binary) + " --profile test --request-id selected agent select " + quoteCommandPart(agent) + " --cwd " + quoteCommandPart(nextDir) + " --model local/fixture --effort low && printf old > old-effect"
			args, _ := json.Marshal(map[string]any{"command": command, "wait_ms": 3000})
			item = map[string]any{"type": "function_call", "id": "fc-select", "call_id": "select", "name": "exec_command", "arguments": string(args), "status": "completed"}
		case 2:
			if len(request.Input) < 4 {
				t.Error("missing selection context")
				return
			}
			last, prior := request.Input[len(request.Input)-1], request.Input[len(request.Input)-2]
			raw, _ := json.Marshal(last)
			if last["role"] != "developer" || prior["type"] != "function_call_output" || !strings.Contains(string(raw), nextDir) {
				t.Error("selection notice did not follow tool result", last, prior)
			}
			args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: new-effect\n+new\n*** End Patch\n"})
			item = map[string]any{"type": "function_call", "id": "fc-patch", "call_id": "patch", "name": "apply_patch", "arguments": string(args), "status": "completed"}
		case 3:
			item = map[string]any{"type": "message", "id": "final", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nselected\n</final>", "annotations": []any{}}}}
		default:
			t.Error("unexpected request", n)
			return
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
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frame("response.created", map[string]any{"response": map[string]any{"id": "r", "status": "in_progress"}})+frame("response.output_item.added", map[string]any{"output_index": 0, "item": start})+frame("response.output_item.done", map[string]any{"output_index": 0, "item": item})+frame("response.completed", map[string]any{"response": map[string]any{"id": "r", "status": "completed", "output": []any{item}}}))
	}))
	defer server.Close()
	configDir := filepath.Join(dir, "config", "zende", "test")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model: local/fixture\neffort: high\nconnections:\n  local:\n    protocol: responses\n    base_url: %s\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n        efforts: [low, high]\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "XDG_") && !strings.HasPrefix(value, "ZENDE_") {
			env = append(env, value)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	for _, service := range []string{"runner", "core"} {
		cmd := exec.CommandContext(ctx, binary, "--profile", "test", service, "serve")
		cmd.Dir, cmd.Env = dir, env
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Error(service, err, log.String())
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error(service, "cleanup timed out")
			}
		})
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
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
	out, err := run("agent", "create", "--cwd", dir, "select then continue")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var accepted protocol.Accepted
	if err := json.Unmarshal(out, &accepted); err != nil {
		t.Fatal(err)
	}
	var snapshot protocol.Snapshot
	for deadline := time.Now().Add(8 * time.Second); ; {
		out, err = run("agent", "inspect", accepted.AgentID)
		if err != nil || json.Unmarshal(out, &snapshot) != nil {
			t.Fatal(err, string(out))
		}
		if len(snapshot.Tasks) == 1 && snapshot.Tasks[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not continue after selection", snapshot, requests.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if requests.Load() != 3 || snapshot.Agent.Cwd != nextDir || strings.TrimSpace(snapshot.Tasks[0].Result) != "selected" {
		t.Fatal(snapshot, requests.Load())
	}
	for path, expected := range map[string]string{filepath.Join(dir, "old-effect"): "old", filepath.Join(nextDir, "new-effect"): "new\n"} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != expected {
			t.Fatal(path, string(raw), err)
		}
	}
	for _, path := range []string{filepath.Join(nextDir, "old-effect"), filepath.Join(dir, "new-effect")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("effect moved to wrong cwd", path, err)
		}
	}
	out, err = run("agent", "history", accepted.AgentID, "--limit", "1000")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var history protocol.EventPage
	if err := json.Unmarshal(out, &history); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range history.Events {
		counts[e.Kind]++
	}
	if counts["agent.selection"] != 1 || counts["model.started"] != 3 || counts["tool.dispatching"] != 2 || counts["task.accepted"] != 1 {
		t.Fatal(counts)
	}
}
