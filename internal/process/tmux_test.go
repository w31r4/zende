package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, code := RunInternalPTY(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func tmuxManager(t *testing.T, limit int64) *Manager {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	m := manager(t, limit)
	t.Cleanup(func() {
		// This socket is derived from this test's unique temporary directory.
		_, _ = tmuxCommand(m.tmux, m.socket, "kill-server")
	})
	return m
}

func TestPTYRealTerminalInstantExitAndIdentity(t *testing.T) {
	m := tmuxManager(t, 65536)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "dir with 'quotes'")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "terminal", Cwd: cwd, PTY: true, Cols: 91, Rows: 31,
		Command: "test -t 0 && test -t 1 && test -t 2 || exit 99; stty size; printf 'hello'; printf ' error' >&2; printf x >> marker; exit 7"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(started, err)
	}
	finished, err := m.Wait(ctx, spec.ID, 5*time.Second)
	if err != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 7 {
		t.Fatalf("%+v %v", finished, err)
	}
	if started.Terminal == nil || started.Terminal.ServerIdentity == "" || started.Identity == "" {
		t.Fatal("missing identities", started)
	}
	out, err := m.Read(ctx, spec.ID, "pty", 0, 65536)
	if err != nil || string(out.Data) != "31 91\r\nhello error" || out.Gap != nil {
		t.Fatalf("output=%q %+v %v", out.Data, out, err)
	}
	again, err := m.Start(ctx, spec)
	if err != nil || again.PID != started.PID || again.Terminal.Pane != started.Terminal.Pane {
		t.Fatal(again, err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, "marker"))
	if err != nil || string(b) != "x" {
		t.Fatal("reexecuted", string(b), err)
	}
	// A caller changing a snapshot must not mutate the manager's identity.
	again.Terminal.Token = "other"
	state, _ := m.Inspect(ctx, spec.ID)
	if state.Terminal.Token == "other" {
		t.Fatal("aliased snapshot")
	}
}

func TestPTYBurstEmptyAndOutputGap(t *testing.T) {
	m := tmuxManager(t, 1024)
	ctx := context.Background()
	for _, tc := range []struct {
		id, command string
		size        int
	}{
		{"empty", "true", 0},
		{"burst", "head -c 262144 /dev/zero", 262144},
	} {
		t.Run(tc.id, func(t *testing.T) {
			_, err := m.Start(ctx, Spec{ID: tc.id, Cwd: t.TempDir(), PTY: true, Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			state, err := m.Wait(ctx, tc.id, 8*time.Second)
			if err != nil || state.Status != "exited" || state.ExitCode == nil || *state.ExitCode != 0 {
				t.Fatal(state, err)
			}
			out, err := m.Read(ctx, tc.id, "pty", 0, 65536)
			if err != nil || out.Total != int64(tc.size) || len(out.Data) != min(tc.size, 1024) {
				t.Fatalf("%+v %v", out, err)
			}
			if tc.size > 1024 && (out.Gap == nil || out.Gap.From != 1024 || out.Gap.To != int64(tc.size)) {
				t.Fatal("missing truncation", out)
			}
			if !bytes.Equal(out.Data, make([]byte, len(out.Data))) {
				t.Fatal("unexpected output")
			}
		})
	}
}

func TestPTYReopensRunningAndCompleted(t *testing.T) {
	m := tmuxManager(t, 8192)
	ctx := context.Background()
	cwd := t.TempDir()
	spec := Spec{ID: "reconnect", Cwd: cwd, PTY: true, Command: "printf before; while [ ! -f finish ]; do sleep 0.01; done; printf after"}
	before, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 8192)
	if err != nil {
		t.Fatal(err)
	}
	still, err := reopened.Wait(ctx, spec.ID, 0)
	if err != nil || still.Status != "running" || still.Identity != before.Identity || still.Terminal.Token != before.Terminal.Token {
		t.Fatal(still, err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	finished, err := reopened.Wait(ctx, spec.ID, 5*time.Second)
	if err != nil || finished.Status != "exited" {
		t.Fatal(finished, err)
	}
	out, err := reopened.Read(ctx, spec.ID, "pty", 0, 8192)
	if err != nil || string(out.Data) != "beforeafter" {
		t.Fatal(out, err)
	}
	third, err := NewManager(m.root, m.shell, 4, 8192)
	if err != nil {
		t.Fatal(err)
	}
	state, err := third.Inspect(ctx, spec.ID)
	if err != nil || state.Status != "exited" || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatal(state, err)
	}
}

func TestPTYInvalidDependencyAndSize(t *testing.T) {
	m := manager(t, 4096)
	m.tmux = "/nonexistent-zende-test-tmux"
	_, err := m.Start(context.Background(), Spec{ID: "dependency", Cwd: t.TempDir(), PTY: true, Command: "touch must-not-run"})
	if err == nil || !strings.Contains(err.Error(), "tmux") {
		t.Fatal(err)
	}
	if len(m.entries) != 0 {
		t.Fatal("dependency failure registered a process")
	}
	for _, spec := range []Spec{
		{ID: "a", Cwd: t.TempDir(), Command: "true", PTY: true, Cols: -1},
		{ID: "b", Cwd: t.TempDir(), Command: "true", Rows: 24},
	} {
		if _, err := m.Start(context.Background(), spec); err == nil {
			t.Fatal("invalid size accepted")
		}
	}
}

func TestPTYMissingStoredRangeIsExplicit(t *testing.T) {
	m := tmuxManager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "missing", Cwd: t.TempDir(), PTY: true, Command: "printf preserved"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, "missing", 5*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	if err := os.Truncate(filepath.Join(m.root, "missing", "pty"), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, "missing", "pty", 0, 4096); err == nil || !strings.Contains(err.Error(), "output_missing") {
		t.Fatal("truncated output was reported as complete", err)
	}
}

func TestPTYServerLifetimeMismatch(t *testing.T) {
	m := tmuxManager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "mismatch", Cwd: t.TempDir(), PTY: true, Command: "read line"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := m.get("mismatch")
	e.mu.Lock()
	e.state.Terminal.ServerIdentity = "different-boot-or-start"
	e.mu.Unlock()
	state, err := m.Inspect(ctx, "mismatch")
	if err != nil || state.Status != "unknown" || state.StdinOpen {
		t.Fatal("wrong server was trusted", state, err)
	}
}

func TestPTYStateDirectoryIsLiteral(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	root := filepath.Join(t.TempDir(), "state '#{pane_id}-%Y'")
	m, err := NewManager(root, "/bin/sh", 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tmuxCommand(m.tmux, m.socket, "kill-server") })
	ctx := context.Background()
	_, err = m.Start(ctx, Spec{ID: "literal", Cwd: t.TempDir(), PTY: true, Command: "printf literal"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, "literal", 5*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	out, err := m.Read(ctx, "literal", "pty", 0, 4096)
	if err != nil || string(out.Data) != "literal" {
		t.Fatal(out, err)
	}
}
