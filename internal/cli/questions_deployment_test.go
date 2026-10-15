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

// Real CLI/core/runner/pipe/UDS, with a local HTTP model fixture. No external
// provider or user installation is involved.
func TestBinaryModelAsksThroughCLIAndContinuesWithAnswer(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("/tmp", "zende-question-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	var requests atomic.Int32
	frame := func(kind string, fields map[string]any) string {
		fields["type"] = kind
		b, _ := json.Marshal(fields)
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
		var item map[string]any
		if requests.Add(1) == 1 {
			if !strings.Contains(request.Instructions, quoteCommandPart(binary)+" --profile 'test'") || !strings.Contains(request.Instructions, "question ask") {
				t.Error("missing executable/profile hint")
			}
			identity := func(label string) string {
				_, rest, ok := strings.Cut(request.Instructions, "\n"+label+": ")
				if !ok {
					return ""
				}
				return strings.SplitN(rest, "\n", 2)[0]
			}
			agent, task := identity("Agent"), identity("Task")
			if agent == "" || task == "" {
				t.Error("missing caller identity")
				return
			}
			command := quoteCommandPart(binary) + " --profile test --request-id question-once question ask " + quoteCommandPart(agent) + " --task " + quoteCommandPart(task) + " 'Choose a file'"
			args, _ := json.Marshal(map[string]any{"command": command, "wait_ms": 3000})
			item = map[string]any{"type": "function_call", "id": "fc", "call_id": "ask-call", "name": "exec_command", "arguments": string(args), "status": "completed"}
		} else {
			if len(request.Input) < 3 {
				t.Error("missing tool and answer")
				return
			}
			last, prior := request.Input[len(request.Input)-1], request.Input[len(request.Input)-2]
			text, _ := json.Marshal(last)
			if last["role"] != "user" || prior["type"] != "function_call_output" || !strings.Contains(string(text), "中文 answer") {
				t.Error("answer not after tool result", string(text), prior)
			}
			item = map[string]any{"type": "message", "id": "m", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nanswer received\n</final>", "annotations": []any{}}}}
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
	config := fmt.Sprintf("model: local/fixture\nconnections:\n  local:\n    protocol: responses\n    base_url: %s\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, s := range os.Environ() {
		if !strings.HasPrefix(s, "XDG_") && !strings.HasPrefix(s, "ZENDE_") {
			env = append(env, s)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	startService := func(service string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "--profile", "test", service, "serve")
		cmd.Dir = dir
		cmd.Env = env
		var log bytes.Buffer
		cmd.Stdout = &log
		cmd.Stderr = &log
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
	startService("runner")
	startService("core")
	run := func(input string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	for _, service := range []string{"runner", "core"} {
		ready := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := run("", service, "status"); err == nil {
				ready = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !ready {
			t.Fatal(service, "not ready")
		}
	}
	out, err := run("", "agent", "create", "Ask before continuing", "--cwd", dir)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var a protocol.Accepted
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	var page protocol.QuestionPage
	for deadline := time.Now().Add(5 * time.Second); ; {
		out, err = run("", "question", "list", a.AgentID)
		if err != nil {
			t.Fatal(err, string(out))
		}
		if err := json.Unmarshal(out, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("model CLI did not create question")
		}
		time.Sleep(10 * time.Millisecond)
	}
	q := page.Items[0]
	if q.TaskID != a.TaskID || q.Prompt != "Choose a file" || requests.Load() != 1 {
		t.Fatal("question attribution or premature request", q, requests.Load())
	}
	out, err = run("中文 answer\nsecond line", "--request-id", "answer-once", "question", "answer", a.AgentID, q.ID, "--file", "-")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var snap protocol.Snapshot
	for deadline := time.Now().Add(5 * time.Second); ; {
		out, err = run("", "agent", "inspect", a.AgentID)
		if err != nil {
			t.Fatal(err, string(out))
		}
		if err := json.Unmarshal(out, &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Tasks) == 1 && snap.Tasks[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not continue", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if requests.Load() != 2 || strings.TrimSpace(snap.Tasks[0].Result) != "answer received" || len(snap.Questions) != 0 {
		t.Fatal(snap, requests.Load())
	}
	out, err = run("", "--request-id", "question-once", "question", "ask", a.AgentID, "--task", a.TaskID, "Choose a file")
	if err != nil {
		t.Fatal("ask retry after completion", err, string(out))
	}
	var retried protocol.Accepted
	if json.Unmarshal(out, &retried) != nil || retried.QuestionID != q.ID {
		t.Fatal("ask retry changed identity")
	}
	out, err = run("", "agent", "history", a.AgentID, "--limit", "1000")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var history protocol.EventPage
	if err := json.Unmarshal(out, &history); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range history.Events {
		counts[event.Kind]++
	}
	for _, kind := range []string{"question.asked", "question.answered", "question.delivered", "tool.dispatching", "task.accepted"} {
		if counts[kind] != 1 {
			t.Fatal("repeated or missing effect", kind, counts[kind])
		}
	}
}
