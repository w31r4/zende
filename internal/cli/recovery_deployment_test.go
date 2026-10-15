package cli

import (
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

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

// Real SIGKILL/core restart, runner, CLI, SQLite and shell effect. The HTTP
// model is a local fixture, not a real-provider or deployment acceptance test.
func TestBinaryRecoversOriginalRunningToolAfterCoreSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary crash integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprintf("pty=%t", pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("tmux required for real PTY integration")
				}
			}
			runBinaryRecoveryWithTUI(t, binary, pty)
		})
	}
}

func runBinaryRecoveryWithTUI(t *testing.T, binary string, pty bool) {
	dir := t.TempDir()
	runDir, err := os.MkdirTemp("", "zende-recovery-")
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
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid fixture request")
			return
		}
		if request["model"] != "fixture" || r.Header.Get("Authorization") != "Bearer TEST_RECOVERY_KEY" {
			t.Error("original binding lost")
		}
		n := requests.Add(1)
		var item map[string]any
		switch n {
		case 1:
			args, _ := json.Marshal(map[string]any{"command": "printf x >> marker; read value; printf '%s' \"$value\"", "pty": pty, "wait_ms": 30000})
			item = map[string]any{"type": "function_call", "id": "fc-start", "call_id": "start", "name": "exec_command", "arguments": string(args), "status": "completed"}
		case 2, 3:
			input, ok := request["input"].([]any)
			if !ok || len(input) == 0 {
				t.Error("missing context")
				return
			}
			last := input[len(input)-1].(map[string]any)
			if last["type"] != "function_call_output" {
				t.Error(last)
				return
			}
			var value tool.ProcessResult
			if json.Unmarshal([]byte(last["output"].(string)), &value) != nil {
				t.Error(last)
				return
			}
			if n == 2 {
				if last["call_id"] != "start" || value.Status != "running" {
					t.Error("original process not rejoined", last)
					return
				}
				args, _ := json.Marshal(map[string]any{"process_id": value.ID, "chars": "done\n", "wait_ms": 3000})
				item = map[string]any{"type": "function_call", "id": "fc-input", "call_id": "input", "name": "write_stdin", "arguments": string(args), "status": "completed"}
			} else {
				if last["call_id"] != "input" || value.Status != "exited" {
					t.Error("original process did not finish", last)
					return
				}
				item = map[string]any{"type": "message", "id": "answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nrecovered original task\n</final>", "annotations": []any{}}}}
			}
		default:
			t.Error("unexpected model request", n)
			return
		}
		start := map[string]any{}
		for key, value := range item {
			start[key] = value
		}
		start["status"] = "in_progress"
		if item["type"] == "function_call" {
			start["arguments"] = ""
		} else {
			start["content"] = []any{}
		}
		response := fmt.Sprintf("r-%d", n)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frame("response.created", map[string]any{"response": map[string]any{"id": response, "status": "in_progress"}})+frame("response.output_item.added", map[string]any{"output_index": 0, "item": start})+frame("response.output_item.done", map[string]any{"output_index": 0, "item": item})+frame("response.completed", map[string]any{"response": map[string]any{"id": response, "status": "completed", "output": []any{item}}}))
	}))
	defer server.Close()
	configDir := filepath.Join(dir, "config", "zende", "test")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model: original/fixture\nconnections:\n  original:\n    protocol: responses\n    base_url: %s\n    credential: env:ZENDE_TEST_MODEL_KEY\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n", server.URL)
	configFile := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_") && !strings.HasPrefix(entry, "ZENDE_") {
			env = append(env, entry)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir, "ZENDE_TEST_MODEL_KEY=TEST_RECOVERY_KEY")
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	type status struct {
		PID int `json:"pid"`
	}
	var pids []int
	var processID string
	var original process.State
	t.Cleanup(func() {
		if processID != "" {
			_, _ = run("process", "terminate", processID, "--force")
			_, _ = run("process", "wait", processID, "--wait-ms", "3000")
		}
		if original.Terminal != nil {
			// Only the private server created by this isolated test profile.
			_ = exec.Command("tmux", "-N", "-S", original.Terminal.Socket, "kill-server").Run()
		}
		for i := len(pids) - 1; i >= 0; i-- {
			pid := pids[i]
			path := filepath.Join("/proc", fmt.Sprint(pid), "exe")
			if exe, _ := os.Readlink(path); exe != binary {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				if exe, _ := os.Readlink(path); exe != binary {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	start := func(service string) int {
		t.Helper()
		out, err := run(service, "start")
		if err != nil {
			t.Fatal(err, string(out))
		}
		var s status
		if json.Unmarshal(out, &s) != nil || s.PID <= 0 {
			t.Fatal(string(out))
		}
		pids = append(pids, s.PID)
		return s.PID
	}
	start("runner")
	corePID := start("core")
	out, err := run("--request-id", "original-task", "agent", "create", "run once then continue")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var accepted protocol.Accepted
	if json.Unmarshal(out, &accepted) != nil {
		t.Fatal(string(out))
	}
	effectObserved := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out, err = run("agent", "history", accepted.AgentID, "--limit", "1000")
		if err != nil {
			t.Fatal(err, string(out))
		}
		var page protocol.EventPage
		if json.Unmarshal(out, &page) != nil {
			t.Fatal(string(out))
		}
		for _, event := range page.Events {
			if event.Kind == "tool.dispatching" {
				var op struct {
					ID string `json:"operation_id"`
				}
				json.Unmarshal(event.Payload, &op)
				processID = op.ID
			}
		}
		if data, err := os.ReadFile(filepath.Join(dir, "marker")); err == nil && string(data) == "x" && processID != "" {
			effectObserved = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !effectObserved || processID == "" || requests.Load() != 1 {
		t.Fatal("tool did not start", processID, requests.Load())
	}
	out, err = run("process", "inspect", processID)
	if err != nil || json.Unmarshal(out, &original) != nil || original.Status != "running" || original.PTY != pty {
		t.Fatal("original process not inspectable", err, string(out))
	}
	t.Logf("started: agent=%s task=%s process=%s pid=%d pty=%t", accepted.AgentID, accepted.TaskID, original.ID, original.PID, pty)
	out, err = run("agent", "activity", accepted.AgentID)
	var activity protocol.Activity
	if err != nil || json.Unmarshal(out, &activity) != nil || len(activity.Items) != 2 || len(activity.Unavailable) != 0 || activity.Truncated {
		t.Fatal("activity did not observe task and process", err, string(out))
	}
	if activity.Items[0].ID != accepted.TaskID || activity.Items[1].ID != processID {
		t.Fatal("activity identity changed", activity)
	}
	ui := startBinaryTUI(t, binary, dir, accepted.AgentID, env)
	ui.detach(t)
	out, err = run("agent", "inspect", accepted.AgentID)
	var active protocol.Snapshot
	if err != nil || json.Unmarshal(out, &active) != nil || len(active.Tasks) != 1 || active.Tasks[0].ID != accepted.TaskID || active.Tasks[0].State != "running" {
		t.Fatal("detach changed original task", err, string(out))
	}
	t.Log("TUI detached; original task still running")
	// A second actual TUI process replays the same history before core crashes.
	ui = startBinaryTUI(t, binary, dir, accepted.AgentID, env)
	if exe, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(corePID), "exe")); exe != binary {
		t.Fatal("core identity changed")
	}
	if err := syscall.Kill(corePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if exe, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(corePID), "exe")); exe != binary {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ui.waitText(t, "offline")
	out, err = run("process", "inspect", processID)
	var retained process.State
	if err != nil || json.Unmarshal(out, &retained) != nil || retained.Identity != original.Identity || retained.PID != original.PID || retained.Status != "running" {
		t.Fatal("core crash changed process", err, string(out))
	}
	t.Log("second TUI attached; core SIGKILL observed as offline; original process still running")
	// Remove current model configuration: only the saved binding can continue.
	if err := os.WriteFile(configFile, []byte("connections: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start("core")
	var snap protocol.Snapshot
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		out, err = run("agent", "inspect", accepted.AgentID)
		if err != nil {
			t.Fatal(err, string(out))
		}
		if json.Unmarshal(out, &snap) != nil {
			t.Fatal(string(out))
		}
		if len(snap.Tasks) == 1 && snap.Tasks[0].State == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != accepted.TaskID || snap.Tasks[0].State != "completed" || strings.TrimSpace(snap.Tasks[0].Result) != "recovered original task" || requests.Load() != 3 {
		t.Fatal(snap, requests.Load())
	}
	ui.waitText(t, "recovered original task")
	ui.detach(t)
	out, err = run("process", "inspect", processID)
	if err != nil || json.Unmarshal(out, &retained) != nil || retained.PID != original.PID || retained.Identity != original.Identity || retained.Status != "exited" {
		t.Fatal("recovery replaced original process", err, string(out))
	}
	out, err = run("agent", "activity", accepted.AgentID)
	if err != nil || json.Unmarshal(out, &activity) != nil || len(activity.Items) != 0 || len(activity.Unavailable) != 0 {
		t.Fatal("finished work still shown as active", err, string(out))
	}
	data, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil || string(data) != "x" {
		t.Fatal("effect replayed", string(data), err)
	}
	out, err = run("agent", "history", accepted.AgentID, "--limit", "1000")
	if err != nil || strings.Contains(string(out), "TEST_RECOVERY_KEY") || !strings.Contains(string(out), "tool.verified") {
		t.Fatal("missing proof or exposed credential", err)
	}
	t.Log("core restarted without current model config; original task completed; TUI reconnected; one effect, three model requests")
}
