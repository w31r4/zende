package process

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

func waitOutput(t *testing.T, m *Manager, id, stream, text string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out, err := m.Read(context.Background(), id, stream, 0, 65536)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out.Data), text) {
			return string(out.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("output did not contain", text)
	return ""
}

func stopTestProcess(t *testing.T, m *Manager, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = m.Signal(context.Background(), protocol.NewID("cleanup"), SignalParams{ID: id, Signal: "KILL"})
		_, _ = m.Wait(context.Background(), id, 3*time.Second)
	})
}

func TestPipeOrderedInputCloseAndDurableDedup(t *testing.T) {
	m := manager(t, 65536)
	ctx := context.Background()
	started, err := m.Start(ctx, Spec{ID: "input", Cwd: t.TempDir(), Command: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "input")
	for i, data := range []string{"first\n", "second\n", "中文\x00bytes"} {
		op := "input-" + strconv.Itoa(i)
		params := InputParams{ID: "input", Data: []byte(data)}
		first, err := m.Input(ctx, op, params)
		if err != nil || first.Written != len(data) {
			t.Fatal(first, err)
		}
		second, err := m.Input(ctx, op, params)
		if err != nil || second.State.InputBytes != first.State.InputBytes {
			t.Fatal(second, err)
		}
	}
	if _, err := m.Resize(ctx, "pipe-resize", ResizeParams{ID: "input", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("pipe resize accepted")
	}
	if _, err := m.Input(ctx, "close", InputParams{ID: "input", Close: true}); err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, "input", 3*time.Second)
	if err != nil || state.Status != "exited" || state.StdinOpen || state.Identity != started.Identity {
		t.Fatal(state, err)
	}
	out, err := m.Read(ctx, "input", "stdout", 0, 65536)
	if err != nil || string(out.Data) != "first\nsecond\n中文\x00bytes" {
		t.Fatal(out, err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Input(ctx, "input-0", InputParams{ID: "input", Data: []byte("first\n")}); err != nil {
		t.Fatal("durable retry", err)
	}
	if _, err := reopened.Input(ctx, "input-0", InputParams{ID: "input", Data: []byte("changed")}); err == nil {
		t.Fatal("conflicting retry accepted")
	}
	if _, err := reopened.Input(ctx, "new-after-close", InputParams{ID: "input", Data: []byte("later")}); err == nil {
		t.Fatal("input after close accepted")
	}
}

func TestPTYInputResizeTargetAndCanonicalEOF(t *testing.T) {
	m := tmuxManager(t, 65536)
	ctx := context.Background()
	for _, id := range []string{"target", "other"} {
		_, err := m.Start(ctx, Spec{ID: id, Cwd: t.TempDir(), PTY: true, Command: "stty -echo; printf ready; read line; stty size; cat; printf eof"})
		if err != nil {
			t.Fatal(err)
		}
		stopTestProcess(t, m, id)
		waitOutput(t, m, id, "pty", "ready")
	}
	before, _ := m.Inspect(ctx, "target")
	if _, err := m.Resize(ctx, "resize-one", ResizeParams{ID: "target", Cols: 101, Rows: 37}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"target", "other"} {
		if _, err := m.Input(ctx, id+"-line", InputParams{ID: id, Data: []byte("go\n")}); err != nil {
			t.Fatal(err)
		}
		want := "24 80"
		if id == "target" {
			want = "37 101"
		}
		waitOutput(t, m, id, "pty", want)
		if _, err := m.Input(ctx, id+"-eof", InputParams{ID: id, Data: []byte("payload"), Close: true}); err != nil {
			t.Fatal(err)
		}
		state, err := m.Wait(ctx, id, 5*time.Second)
		if err != nil || state.Status != "exited" || state.StdinOpen || state.ExitCode == nil || *state.ExitCode != 0 {
			t.Fatal(state, err)
		}
		waitOutput(t, m, id, "pty", "payloadeof")
		if id == "target" && (state.Identity != before.Identity || state.Terminal.Cols != 101 || state.Terminal.Rows != 37) {
			t.Fatal("resize changed process", state)
		}
	}
}

func TestPTYRawCloseIsExplicitlyUnsupported(t *testing.T) {
	m := tmuxManager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "raw", Cwd: t.TempDir(), PTY: true, Command: "stty raw -echo; printf ready; sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "raw")
	waitOutput(t, m, "raw", "pty", "ready")
	if _, err := m.Input(ctx, "raw-close", InputParams{ID: "raw", Close: true}); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatal(err)
	}
	state, _ := m.Inspect(ctx, "raw")
	if !state.StdinOpen {
		t.Fatal("raw close falsely reported")
	}
}

func TestControlPendingIsNotReplayedOrBypassed(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "pending", Cwd: t.TempDir(), Command: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "pending")
	p := InputParams{ID: "pending", Data: []byte("not-replayed")}
	req, _ := protocol.NewRequest("process.stdin", "uncertain", p)
	fingerprint, _ := req.Fingerprint()
	dir := filepath.Join(m.root, "pending", "controls")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(dir, "uncertain.json"), ControlRecord{Version: 1, OperationID: "uncertain", Fingerprint: fingerprint, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"uncertain", "different-id"} {
		if _, err := m.Input(ctx, op, p); err == nil || !strings.Contains(err.Error(), "operation_unknown") {
			t.Fatal(err)
		}
	}
	out, err := m.Read(ctx, "pending", "stdout", 0, 4096)
	if err != nil || len(out.Data) != 0 {
		t.Fatal("pending input replayed", out, err)
	}
	record, err := m.ControlInspect(ctx, ControlTarget{ID: "pending", OperationID: "uncertain"})
	if err != nil || record.Status != "pending" {
		t.Fatal(record, err)
	}
	if _, err := m.Signal(ctx, "stop-uncertain", SignalParams{ID: "pending", Signal: "TERM"}); err != nil {
		t.Fatal("cannot stop uncertain process", err)
	}
}

func TestPipeWriteTimeoutIsBoundedAndReportsPartial(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "blocked-input", Cwd: t.TempDir(), Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "blocked-input")
	e, _ := m.get("blocked-input")
	raw, err := e.stdin.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var resizeErr error
	if err := raw.Control(func(fd uintptr) { _, resizeErr = unix.FcntlInt(fd, unix.F_SETPIPE_SZ, 4096) }); err != nil {
		t.Fatal(err)
	}
	if resizeErr != nil {
		t.Fatal(resizeErr)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	result, err := m.Input(short, "bounded-write", InputParams{ID: "blocked-input", Data: make([]byte, MaxInputBytes)})
	if err == nil || result.Written >= MaxInputBytes || !strings.Contains(err.Error(), "input_partial") {
		t.Fatal(result, err)
	}
	state, err := m.Wait(ctx, "blocked-input", 0)
	if err != nil || state.Status != "running" {
		t.Fatal("timeout killed command", state, err)
	}
}

func TestSignalCancelAndOwnedGroups(t *testing.T) {
	ctx := context.Background()
	t.Run("cancel", func(t *testing.T) {
		m := manager(t, 4096)
		_, err := m.Start(ctx, Spec{ID: "cancel", Cwd: t.TempDir(), Command: "trap 'exit 23' INT; printf ready; while :; do sleep 0.05; done"})
		if err != nil {
			t.Fatal(err)
		}
		stopTestProcess(t, m, "cancel")
		waitOutput(t, m, "cancel", "stdout", "ready")
		if _, err := m.Signal(ctx, "interrupt", SignalParams{ID: "cancel", Signal: "INT"}); err != nil {
			t.Fatal(err)
		}
		state, err := m.Wait(ctx, "cancel", 5*time.Second)
		if err != nil || state.ExitCode == nil || *state.ExitCode != 23 || state.CancelSignal != "INT" {
			t.Fatal(state, err)
		}
	})
	for _, pty := range []bool{false, true} {
		t.Run(strconv.FormatBool(pty), func(t *testing.T) {
			m := manager(t, 4096)
			if pty {
				m = tmuxManager(t, 4096)
			}
			command := "sleep 30 & echo $!; wait"
			if pty {
				command = "exec /bin/bash -c 'set -m; sleep 30 & echo $!; wait'"
			}
			original, err := m.Start(ctx, Spec{ID: "group", Cwd: t.TempDir(), PTY: pty, Command: command})
			if err != nil {
				t.Fatal(err)
			}
			stopTestProcess(t, m, "group")
			stream := "stdout"
			if pty {
				stream = "pty"
			}
			text := waitOutput(t, m, "group", stream, "\n")
			child, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil {
				t.Fatal(text, err)
			}
			pg, err := syscall.Getpgid(child)
			if err != nil {
				t.Fatal(err)
			}
			if pty && pg == original.PID {
				t.Fatal("fixture did not create separate job group")
			}
			if _, err := m.Signal(ctx, "terminate-group", SignalParams{ID: "group", Signal: "KILL"}); err != nil {
				t.Fatal(err)
			}
			state, err := m.Wait(ctx, "group", 5*time.Second)
			if err != nil || state.Status != "exited" || state.CancelSignal != "KILL" {
				if state.Terminal != nil {
					pane, paneErr := inspectPane(m.tmux, *state.Terminal)
					e, _ := m.get("group")
					var capture captureRecord
					captureErr := readJSON(filepath.Join(e.dir, "capture.json"), &capture)
					t.Logf("pane=%+v pane_error=%v capture=%+v capture_error=%v leader_identity=%q", pane, paneErr, capture, captureErr, identity(state.PID))
					for _, pid := range []int{state.PID, child, state.Terminal.ServerPID} {
						stat, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
						wchan, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "wchan"))
						t.Logf("pid=%d stat=%s wchan=%s", pid, stat, wchan)
					}
				}
				t.Fatal(state, err)
			}
			if b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat")); err == nil {
				rest := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
				if len(rest) == 0 || rest[0] != "Z" {
					t.Fatal("child is still running", string(b))
				}
			}
		})
	}
}
