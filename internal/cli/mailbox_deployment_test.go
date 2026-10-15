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
	"zende.local/harness/internal/render"
)

// Actual binary/SQLite/UDS, two SIGKILL windows, local HTTP model fixture.
func TestBinaryMailboxSurvivesAcceptanceAndDeliveryCrashes(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("", "zende-mail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	var requests atomic.Int32
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(input.Input)
		if bytes.Count(raw, []byte("durable-peer-evidence")) != 1 {
			t.Error("message missing/duplicated", string(raw))
			http.Error(w, "fixture", 500)
			return
		}
		n := requests.Add(1)
		if n == 1 {
			arrived <- struct{}{}
			<-r.Context().Done()
			return
		}
		if n != 2 {
			t.Error("duplicate processing", n)
			http.Error(w, "fixture", 500)
			return
		}
		frame := func(kind string, v map[string]any) string {
			v["type"] = kind
			b, _ := json.Marshal(v)
			return "data: " + string(b) + "\n\n"
		}
		item := map[string]any{"type": "message", "id": "answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nreceived\n</final>", "annotations": []any{}}}}
		start := map[string]any{"type": "message", "id": "answer", "role": "assistant", "status": "in_progress", "content": []any{}}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frame("response.created", map[string]any{"response": map[string]any{"id": "reply", "status": "in_progress"}})+
			frame("response.output_item.added", map[string]any{"output_index": 0, "item": start})+
			frame("response.output_item.done", map[string]any{"output_index": 0, "item": item})+
			frame("response.completed", map[string]any{"response": map[string]any{"id": "reply", "status": "completed", "output": []any{item}}}))
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
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "XDG_") && !strings.HasPrefix(v, "ZENDE_") {
			env = append(env, v)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir)
	run := func(input string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	must := func(target any, input string, args ...string) {
		t.Helper()
		out, err := run(input, args...)
		if err != nil || bytes.Contains(out, []byte{27}) {
			t.Fatal(args, err, string(out))
		}
		if err := json.Unmarshal(out, target); err != nil {
			t.Fatal(err, string(out))
		}
	}
	var pids []int
	defer func() {
		for _, pid := range pids {
			path := filepath.Join("/proc", fmt.Sprint(pid), "exe")
			if exe, _ := os.Readlink(path); exe != binary {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if exe, _ := os.Readlink(path); exe != binary {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if exe, _ := os.Readlink(path); exe == binary {
				t.Errorf("test core did not stop: %d", pid)
			}
		}
	}()
	start := func() int {
		var s struct {
			PID int `json:"pid"`
		}
		must(&s, "", "core", "start")
		if s.PID <= 0 {
			t.Fatal(s)
		}
		pids = append(pids, s.PID)
		return s.PID
	}
	kill := func(pid int) {
		t.Helper()
		path := filepath.Join("/proc", fmt.Sprint(pid), "exe")
		if exe, _ := os.Readlink(path); exe != binary {
			t.Fatal("test PID identity changed")
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if exe, _ := os.Readlink(path); exe != binary {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("test core did not exit")
	}
	pid := start()
	var agent, control, sent protocol.Accepted
	must(&agent, "", "agent", "create", "--cwd", dir)
	must(&control, "", "agent", "stop", agent.AgentID)
	sendArgs := []string{"--request-id", "stable-send", "agent", "send", agent.AgentID, "--file", "-", "--delivery", "steer"}
	must(&sent, "durable-peer-evidence", sendArgs...)
	var m protocol.Mail
	must(&m, "", "mailbox", "inspect", sent.MessageID)
	if m.State != "accepted" || m.TaskID != "" {
		t.Fatal("paused delivery", m)
	}
	kill(pid)
	pid = start()
	var retry protocol.Accepted
	must(&retry, "durable-peer-evidence", sendArgs...)
	if retry != sent {
		t.Fatal("accepted retry changed", retry, sent)
	}
	var snap protocol.Snapshot
	must(&snap, "", "agent", "inspect", agent.AgentID)
	if !snap.Agent.Paused || len(snap.Tasks) != 0 || requests.Load() != 0 {
		t.Fatal("restart woke paused mail", snap)
	}
	must(&control, "", "agent", "resume", agent.AgentID)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not receive mail")
	}
	must(&m, "", "mailbox", "inspect", sent.MessageID)
	if m.State != "delivered" || m.DeliveredSeq == 0 || m.TaskID != control.TaskID {
		t.Fatal(m)
	}
	delivered := m
	kill(pid)
	start()
	for deadline := time.Now().Add(8 * time.Second); ; {
		must(&m, "", "mailbox", "inspect", sent.MessageID)
		if m.State == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery not recovered", m)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.TaskID != delivered.TaskID || m.DeliveredSeq != delivered.DeliveredSeq || requests.Load() != 2 {
		t.Fatal("recovery changed delivery", m, delivered)
	}
	must(&retry, "durable-peer-evidence", sendArgs...)
	if retry != sent {
		t.Fatal(retry)
	}
	must(&snap, "", "agent", "inspect", agent.AgentID)
	if len(snap.Tasks) != 1 || snap.Tasks[0].State != "completed" {
		t.Fatal("duplicate task", snap)
	}
	var history protocol.EventPage
	must(&history, "", "agent", "history", agent.AgentID, "--limit", "1000")
	count := 0
	projection := render.Transcript{Agent: protocol.Agent{ID: agent.AgentID}}
	for _, event := range history.Events {
		if err := projection.Apply(event); err != nil {
			t.Fatal("CLI history cannot be replayed by TUI", event.Kind, err)
		}
		if event.Kind == "mailbox.delivered" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate context delivery", count)
	}
	view := strings.Join(projection.Lines(80, 24, 0, false), "\n")
	if strings.Count(view, "durable-peer-evidence") != 1 || !strings.Contains(view, "@ user · steer · processed") || strings.Contains(view, "Handle mailbox message") {
		t.Fatal("duplicate or unmarked mailbox input", view)
	}
	t.Log("accepted while paused; SIGKILL preserves pause; second SIGKILL after delivery resumes original task; one message and delivery")
}
