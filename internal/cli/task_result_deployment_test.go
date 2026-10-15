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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/render"
)

// Real binaries, UDS, SQLite and pipe runner; only model replies are fixtures.
func TestBinaryTaskWaitRecoversCLIOutputBeforeContext(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	socketDir, err := os.MkdirTemp("", "zende-wait-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	childGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(childGate) }) }
	defer releaseChild()
	var parents, children atomic.Int32
	acceptedFile := filepath.Join(dir, "spawn.json")
	waitingFile, waitedFile := filepath.Join(dir, "waiting"), filepath.Join(dir, "waited.json")
	releaseFile, effectFile := filepath.Join(dir, "release"), filepath.Join(dir, "effects")
	prefix := quoteCommandPart(binary) + " --profile test "
	frame := func(kind string, v map[string]any) string {
		v["type"] = kind
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		child := false
		for _, item := range request.Input {
			if item["role"] == "user" {
				raw, _ := json.Marshal(item["content"])
				child = strings.Contains(string(raw), "child-only-work")
				break
			}
		}
		var item map[string]any
		if child {
			if children.Add(1) != 1 {
				t.Error("child rerun")
			}
			select {
			case <-childGate:
			case <-r.Context().Done():
				return
			}
			item = map[string]any{"type": "message", "id": "child-answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nunique-child-evidence 中文\n</final>", "annotations": []any{}}}}
		} else {
			n := parents.Add(1)
			command := ""
			switch n {
			case 1:
				command = prefix + "agent spawn --context none child-only-work > " + quoteCommandPart(acceptedFile)
			case 2:
				raw, err := os.ReadFile(acceptedFile)
				var a protocol.Accepted
				if err != nil || json.Unmarshal(raw, &a) != nil || a.AgentID == "" || a.TaskID == "" {
					t.Error("missing spawn receipt")
					return
				}
				command = "printf once >> " + quoteCommandPart(effectFile) + "; touch " + quoteCommandPart(waitingFile) + "; " +
					prefix + "agent wait " + quoteCommandPart(a.AgentID) + " --task " + quoteCommandPart(a.TaskID) + " --wait-ms 30000 > " + quoteCommandPart(waitedFile) +
					"; while [ ! -e " + quoteCommandPart(releaseFile) + " ]; do sleep 0.02; done"
			case 3:
				raw, _ := json.Marshal(request.Input)
				if bytes.Count(raw, []byte("unique-child-evidence")) != 1 || !bytes.Contains(raw, []byte("peer output")) {
					t.Error("wait body missing or duplicated", string(raw))
					return
				}
				last := request.Input[len(request.Input)-1]
				if last["type"] != "function_call_output" {
					t.Error("queue, not original tool, delivered result", last)
					return
				}
				item = map[string]any{"type": "message", "id": "parent-answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\nparent done\n</final>", "annotations": []any{}}}}
			default:
				t.Error("parent woke again", n)
				http.Error(w, "unexpected request", 500)
				return
			}
			if command != "" {
				args, _ := json.Marshal(map[string]any{"command": command, "pty": false, "wait_ms": 30000})
				item = map[string]any{"type": "function_call", "id": fmt.Sprintf("item-%d", n), "call_id": fmt.Sprintf("call-%d", n), "name": "exec_command", "arguments": string(args), "status": "completed"}
			}
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
		io.WriteString(w, frame("response.created", map[string]any{"response": map[string]any{"id": "reply", "status": "in_progress"}})+
			frame("response.output_item.added", map[string]any{"output_index": 0, "item": start})+
			frame("response.output_item.done", map[string]any{"output_index": 0, "item": item})+
			frame("response.completed", map[string]any{"response": map[string]any{"id": "reply", "status": "completed", "output": []any{item}}}))
	}))
	t.Cleanup(server.Close)
	configDir := filepath.Join(dir, "config", "zende", "test")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model: local/fixture\nconnections:\n  local:\n    protocol: responses\n    base_url: %s\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "XDG_") && !strings.HasPrefix(value, "ZENDE_") {
			env = append(env, value)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+socketDir)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
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
	type service struct {
		cmd     *exec.Cmd
		done    chan error
		log     bytes.Buffer
		stopped bool
	}
	start := func(name string) *service {
		t.Helper()
		s := &service{cmd: exec.CommandContext(ctx, binary, "--profile", "test", name, "serve"), done: make(chan error, 1)}
		s.cmd.Env, s.cmd.Dir, s.cmd.Stdout, s.cmd.Stderr = env, dir, &s.log, &s.log
		if err := s.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { s.done <- s.cmd.Wait() }()
		t.Cleanup(func() {
			if s.stopped {
				return
			}
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-s.done:
				if err != nil {
					t.Error(name, err, s.log.String())
				}
			case <-time.After(5 * time.Second):
				_ = s.cmd.Process.Kill()
				<-s.done
				t.Error(name, "stop timeout")
			}
		})
		for deadline := time.Now().Add(5 * time.Second); ; {
			if _, err := run(name, "status"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(name, "not ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return s
	}
	// Cleanups stop services before cancelling ctx. A killed core is reaped and
	// marked stopped; only owned command PIDs are used.
	start("runner")
	core := start("core")
	defer func() { releaseChild(); _ = os.WriteFile(releaseFile, nil, 0600) }()
	var parent protocol.Accepted
	must(&parent, "agent", "create", "--cwd", dir, "parent-work")
	poll := func(check func() bool) {
		t.Helper()
		for deadline := time.Now().Add(8 * time.Second); ; {
			if check() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("fixture condition timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	poll(func() bool { _, err := os.Stat(waitingFile); return err == nil })
	releaseChild()
	var observed protocol.TaskOutcome
	poll(func() bool {
		raw, err := os.ReadFile(waitedFile)
		return err == nil && json.Unmarshal(raw, &observed) == nil && observed.Terminal
	})
	if observed.Result == nil || observed.MessageID == "" {
		t.Fatal(observed)
	}
	var mail protocol.Mail
	must(&mail, "mailbox", "inspect", observed.MessageID)
	if mail.State != "accepted" || mail.DeliveredSeq != 0 || mail.TaskID != "" {
		t.Fatal("CLI output prematurely consumed result", mail)
	}
	var before protocol.Snapshot
	must(&before, "agent", "inspect", parent.AgentID)
	if len(before.Tasks) != 1 || before.Tasks[0].State != "running" {
		t.Fatal(before)
	}
	if err := core.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-core.done; err == nil {
		t.Fatal("expected SIGKILL")
	}
	core.stopped = true
	if err := os.WriteFile(releaseFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	start("core")
	poll(func() bool { must(&mail, "mailbox", "inspect", observed.MessageID); return mail.State == "processed" })
	var after protocol.Snapshot
	must(&after, "agent", "inspect", parent.AgentID)
	if mail.TaskID != parent.TaskID || mail.DeliveredSeq == 0 || len(after.Tasks) != 1 || after.Tasks[0].State != "completed" || parents.Load() != 3 || children.Load() != 1 {
		t.Fatal(mail, after, parents.Load(), children.Load())
	}
	effects, err := os.ReadFile(effectFile)
	if err != nil || string(effects) != "once" {
		t.Fatal("command replayed", string(effects), err)
	}
	var page protocol.EventPage
	must(&page, "agent", "history", parent.AgentID, "--limit", "1000")
	deliveries, contexts := 0, 0
	projection := render.Transcript{Agent: protocol.Agent{ID: parent.AgentID}}
	for _, event := range page.Events {
		if err := projection.Apply(event); err != nil {
			t.Fatal(event.Kind, err)
		}
		if event.Kind == "mailbox.delivered" {
			deliveries++
		}
		if event.Kind == "context.message" && bytes.Contains(event.Payload, []byte("unique-child-evidence")) {
			contexts++
		}
	}
	if deliveries != 1 || contexts != 1 {
		t.Fatal("duplicate durable delivery", deliveries, contexts)
	}
	view := strings.Join(projection.Lines(80, 24, 0, false), "\n")
	if strings.Count(view, "unique-child-evidence") != 1 || !strings.Contains(view, "@ "+observed.AgentID) {
		t.Fatal("lost or duplicate TUI body", view)
	}
	t.Log("CLI printed completed reference; SIGKILL before tool context; same task/process resumed; child body delivered once; effect once")
}
