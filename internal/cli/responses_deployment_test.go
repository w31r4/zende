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

// Real executable/config/UDS/store/patch, but the provider is an HTTP fixture.
// This is wiring verification, not a real model or deployment acceptance test.
func TestConfiguredResponsesBinaryExecutesPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("", "zende-responses-test-")
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
		if r.Header.Get("Authorization") != "Bearer TEST_ONLY_KEY" {
			t.Error("configured key not resolved")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request["model"] != "fixture" {
			t.Error(request["model"])
		}
		var item map[string]any
		if requests.Add(1) == 1 {
			args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: created.txt\n+<local>fixture & text</local>\n*** End Patch\n"})
			// Simulate an API emitting literal HTML inside argument JSON; store
			// serialization will escape it before the next model request.
			args = bytes.ReplaceAll(args, []byte(`\u003c`), []byte("<"))
			args = bytes.ReplaceAll(args, []byte(`\u003e`), []byte(">"))
			args = bytes.ReplaceAll(args, []byte(`\u0026`), []byte("&"))
			item = map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "apply_patch", "arguments": string(args), "status": "completed"}
		} else {
			input := request["input"].([]any)
			last := input[len(input)-1].(map[string]any)
			if last["type"] != "function_call_output" || last["call_id"] != "call" {
				t.Error("missing patch result", last)
			}
			item = map[string]any{"type": "message", "id": "m", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "<final>\npatch saved\n</final>", "annotations": []any{}}}}
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
	config := fmt.Sprintf("model: test/fixture\nconnections:\n  test:\n    protocol: responses\n    base_url: %s\n    credential: env:ZENDE_TEST_MODEL_KEY\n    models:\n      - id: fixture\n        context_window: 32000\n        max_output: 4096\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, s := range os.Environ() {
		if !strings.HasPrefix(s, "XDG_") && !strings.HasPrefix(s, "ZENDE_") {
			env = append(env, s)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir, "ZENDE_TEST_MODEL_KEY=TEST_ONLY_KEY")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	core := exec.CommandContext(ctx, binary, "--profile", "test", "core", "serve")
	core.Dir = dir
	core.Env = env
	var log bytes.Buffer
	core.Stdout = &log
	core.Stderr = &log
	if err := core.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- core.Wait() }()
	defer func() {
		_ = core.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-stopped:
			if err != nil {
				t.Error(err, log.String())
			}
		case <-time.After(5 * time.Second):
			_ = core.Process.Kill()
			<-stopped
			t.Error("test core did not stop")
		}
	}()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	ready := false
	for i := 0; i < 100; i++ {
		if _, err := run("core", "status"); err == nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("test core did not start")
	}
	out, err := run("--request-id", "fixture-once", "agent", "create", "create the fixture file")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var accepted protocol.Accepted
	if err := json.Unmarshal(out, &accepted); err != nil {
		t.Fatal(err)
	}
	var snap protocol.Snapshot
	completed := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out, err = run("agent", "inspect", accepted.AgentID)
		if err != nil {
			t.Fatal(err, string(out))
		}
		if err := json.Unmarshal(out, &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Tasks) == 1 && snap.Tasks[0].State == "completed" {
			completed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !completed || requests.Load() != 2 || strings.TrimSpace(snap.Tasks[0].Result) != "patch saved" {
		t.Fatal(snap, requests.Load())
	}
	data, err := os.ReadFile(filepath.Join(dir, "created.txt"))
	if err != nil || string(data) != "<local>fixture & text</local>\n" {
		t.Fatal(string(data), err)
	}
	out, err = run("agent", "history", accepted.AgentID, "--limit", "1000")
	if err != nil || strings.Contains(string(out), "TEST_ONLY_KEY") {
		t.Fatal("history/secret boundary", err)
	}
}
