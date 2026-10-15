package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func TestMain(m *testing.M) {
	if handled, code := process.RunInternalPTY(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func toolRunner(t *testing.T) (*process.Manager, *Registry) {
	t.Helper()
	m, err := process.NewManager(t.TempDir(), "/bin/sh", 8, 4096)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(ProcessExecutors(m)...)
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}
func executeProcessTool(t *testing.T, m *process.Manager, r *Registry, name string, call Call) (ProcessResult, error) {
	t.Helper()
	e, _, ok := r.Lookup(name)
	if !ok {
		t.Fatal(name)
	}
	result, err := e.Execute(context.Background(), call)
	if name == "exec_command" && result.ProcessID != "" {
		t.Cleanup(func() {
			state, inspectErr := m.Inspect(context.Background(), result.ProcessID)
			if inspectErr != nil {
				return
			}
			if state.Status == "running" || state.Status == "unknown" {
				_, _ = m.Signal(context.Background(), protocol.NewID("cleanup"), process.SignalParams{ID: state.ID, Signal: "KILL"})
				_, _ = m.Wait(context.Background(), state.ID, time.Second)
			}
			if state.Terminal != nil {
				_ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").Run()
			}
		})
	}
	var output ProcessResult
	if len(result.Content) > 0 {
		if decodeErr := json.Unmarshal([]byte(result.Content[0].Text), &output); decodeErr != nil {
			t.Fatal(result, decodeErr)
		}
	}
	return output, err
}
func arguments(t *testing.T, p any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProcessToolsContinueOriginalPipeAndDeduplicate(t *testing.T) {
	m, r := toolRunner(t)
	cwd := t.TempDir()
	call := Call{ID: "original", AgentID: "agent", TaskID: "task", Cwd: cwd, Arguments: arguments(t, map[string]any{"command": "printf x >> marker; read value; printf '<%s>' \"$value\"", "wait_ms": 0})}
	started, err := executeProcessTool(t, m, r, "exec_command", call)
	if err != nil || started.ID != "original" || started.Status != "running" || !started.StdinOpen || len(started.Output) != 2 {
		t.Fatal(started, err)
	}
	state, err := m.Inspect(context.Background(), started.ID)
	if err != nil || state.AgentID != "agent" || state.TaskID != "task" || !state.References["task:task"].Held {
		t.Fatal(state, err)
	}
	write := Call{ID: "write-once", Arguments: arguments(t, map[string]any{"process_id": started.ID, "chars": "hello\n", "close_stdin": true, "wait_ms": 3000})}
	for range 2 {
		finished, err := executeProcessTool(t, m, r, "write_stdin", write)
		if err != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 0 || finished.Written == nil || *finished.Written != 6 || finished.Output[0].Text != "<hello>" {
			t.Fatal(finished, err)
		}
	}
	again, err := executeProcessTool(t, m, r, "exec_command", call)
	if err != nil || again.Status != "exited" || again.ID != started.ID {
		t.Fatal(again, err)
	}
	marker, err := os.ReadFile(filepath.Join(cwd, "marker"))
	if err != nil || string(marker) != "x" {
		t.Fatal("command repeated", string(marker), err)
	}
}

func TestProcessToolOutputCursorsAndControlEscaping(t *testing.T) {
	m, r := toolRunner(t)
	page, err := executeProcessTool(t, m, r, "exec_command", Call{ID: "pages", Cwd: t.TempDir(), Arguments: arguments(t, map[string]any{"command": "printf '12345678'; printf '\033[2Jerror' >&2; exit 7", "wait_ms": 3000, "max_output_bytes": 3})})
	if err != nil || page.Status != "exited" || page.ExitCode == nil || *page.ExitCode != 7 || page.Output[0].Text != "123" || page.Output[0].Next != 3 || page.Output[1].Text != "\\x1b[2" {
		t.Fatal(page, err)
	}
	next, err := executeProcessTool(t, m, r, "write_stdin", Call{ID: "read-only", Arguments: arguments(t, map[string]any{"process_id": page.ID, "offsets": map[string]int64{"stdout": 3, "stderr": 3}, "wait_ms": 0, "max_output_bytes": 3})})
	if err != nil || next.Written != nil || next.Output[0].Text != "456" || next.Output[0].Next != 6 || next.Output[1].Text != "Jer" {
		t.Fatal(next, err)
	}
	state, _ := m.Inspect(context.Background(), page.ID)
	if state.InputBytes != 0 {
		t.Fatal("read sent input", state)
	}
	page, err = executeProcessTool(t, m, r, "exec_command", Call{ID: "gap", TaskID: "held", Cwd: t.TempDir(), Arguments: arguments(t, map[string]any{"command": "head -c 8192 /dev/zero", "wait_ms": 3000, "max_output_bytes": 65536})})
	if err != nil || page.Output[0].Gap == nil || page.Output[0].Gap.From != 4096 || page.Output[0].Gap.To != 8192 || page.Output[0].Next != 8192 || strings.ContainsRune(page.Output[0].Text, 0) {
		t.Fatal(page, err)
	}
}

func TestProcessToolsValidateBeforeStartingOrWriting(t *testing.T) {
	m, r := toolRunner(t)
	for i, raw := range []string{
		`{"command":"touch should-not-exist","wait_ms":30001}`,
		`{"command":"true","max_output_bytes":0}`,
		`{"command":"true","cols":80}`,
		`{"command":"true","pty":true,"cols":0}`,
		`{"command":"true","pty":null}`,
		`{"command":"true","command":"false"}`,
		`{"COMMAND":"true"}`,
		`{"command":"true","unknown":1}`,
	} {
		id := protocol.NewID("invalid")
		if _, err := executeProcessTool(t, m, r, "exec_command", Call{ID: id, Cwd: t.TempDir(), Arguments: []byte(raw)}); err == nil {
			t.Fatal(i, "invalid tool arguments accepted")
		}
		if _, err := m.Inspect(context.Background(), id); err == nil {
			t.Fatal("invalid request dispatched", i)
		}
	}
	started, err := executeProcessTool(t, m, r, "exec_command", Call{ID: "input-validation", Cwd: t.TempDir(), Arguments: arguments(t, map[string]any{"command": "cat", "wait_ms": 0})})
	if err != nil {
		t.Fatal(err)
	}
	for _, offsets := range []any{map[string]any{"stdout": 999}, map[string]any{"pty": 0}, map[string]any{"stdout": nil}} {
		_, err := executeProcessTool(t, m, r, "write_stdin", Call{ID: protocol.NewID("bad-write"), Arguments: arguments(t, map[string]any{"process_id": started.ID, "chars": "must not write", "offsets": offsets})})
		if err == nil {
			t.Fatal("invalid offsets accepted")
		}
	}
	state, _ := m.Inspect(context.Background(), started.ID)
	if state.InputBytes != 0 {
		t.Fatal("validation happened after input", state)
	}
}

func TestProcessToolPTYAndRelativeCwd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	m, r := toolRunner(t)
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	page, err := executeProcessTool(t, m, r, "exec_command", Call{ID: "terminal-tool", Cwd: cwd, Arguments: arguments(t, map[string]any{"command": "test -t 0 && test -t 1 && test -t 2 || exit 99; stty -echo; stty size; read value; printf 'got:%s' \"$value\"", "cwd": "child", "pty": true, "cols": 88, "rows": 29, "wait_ms": 100})})
	if err != nil || !page.PTY || page.Status != "running" || len(page.Output) != 1 || !strings.Contains(page.Output[0].Text, "29 88") {
		t.Fatal(page, err)
	}
	state, _ := m.Inspect(context.Background(), page.ID)
	if state.Cwd != filepath.Join(cwd, "child") {
		t.Fatal(state)
	}
	finished, err := executeProcessTool(t, m, r, "write_stdin", Call{ID: "terminal-input", Arguments: arguments(t, map[string]any{"process_id": page.ID, "chars": "ok\n", "offsets": map[string]int64{"pty": page.Output[0].Next}, "wait_ms": 3000})})
	if err != nil || finished.Status != "exited" || finished.Output[0].Text != "got:ok" || finished.Written == nil || *finished.Written != 3 {
		t.Fatal(finished, err)
	}
}

type observationFailure struct{ process.Controller }

func (o observationFailure) Wait(context.Context, string, time.Duration) (process.State, error) {
	return process.State{}, protocol.E("unavailable", "test observation failure")
}

func TestObservationFailureRetainsAcceptedProcessIdentity(t *testing.T) {
	m, _ := toolRunner(t)
	r, err := NewRegistry(ProcessExecutors(observationFailure{m})...)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeProcessTool(t, m, r, "exec_command", Call{ID: "uncertain-observation", Cwd: t.TempDir(), Arguments: arguments(t, map[string]any{"command": "read value", "wait_ms": 0})})
	if err == nil || !strings.Contains(err.Error(), "operation_unknown") || result.ID != "uncertain-observation" {
		t.Fatal(result, err)
	}
	state, err := m.Inspect(context.Background(), result.ID)
	if err != nil || state.Status != "running" {
		t.Fatal(state, err)
	}
}

func TestModelProjectionFitsToolResultBudget(t *testing.T) {
	state := process.State{Spec: process.Spec{ID: "budget"}}
	text := process.DisplayText(make([]byte, MaxReadBytes))
	result := processResult(state, nil, []ProcessPage{{Stream: "stdout", Text: text}, {Stream: "stderr", Text: text}})
	b, err := json.Marshal(result)
	if err != nil || len(b) > model.MaxBlockBytes {
		t.Fatal(len(b), err)
	}
}

type cancelledObservation struct {
	process.Controller
	failure error
}

func (c cancelledObservation) Wait(context.Context, string, time.Duration) (process.State, error) {
	return process.State{}, c.failure
}

func TestCancellationDoesNotHideObservationCorruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, failure := range []error{context.Canceled, protocol.E("transport_unknown", "closed"), protocol.E("output_corrupt", "corrupt record")} {
		p := processTool{runner: cancelledObservation{failure: failure}}
		result, err := p.observe(ctx, process.State{Spec: process.Spec{ID: "known-start"}, Status: "running"}, nil, nil, 0, 1024, true)
		var output ProcessResult
		json.Unmarshal([]byte(result.Content[0].Text), &output)
		corrupt := strings.Contains(failure.Error(), "output_corrupt")
		if corrupt && (err == nil || output.ObservationInterrupted) || !corrupt && (err != nil || !output.ObservationInterrupted) {
			t.Fatal(output, err)
		}
	}
}
