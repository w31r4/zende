package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func manager(t *testing.T, limit int64) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "/bin/sh", 4, limit)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPipeCaptureBeforeInstantExitAndIdempotency(t *testing.T) {
	m := manager(t, 65536)
	ctx := context.Background()
	cwd := t.TempDir()
	spec := Spec{ID: "instant", Cwd: cwd, Command: "printf hello; printf error >&2; printf x >> marker"}
	started, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := m.Wait(ctx, spec.ID, 3*time.Second)
	if err != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatal(finished, err)
	}
	for stream, want := range map[string]string{"stdout": "hello", "stderr": "error"} {
		out, err := m.Read(ctx, spec.ID, stream, 0, 65536)
		if err != nil || string(out.Data) != want || out.Next != int64(len(want)) {
			t.Fatal(out, err)
		}
	}
	again, err := m.Start(ctx, spec)
	if err != nil || again.PID != started.PID || again.InstanceID != started.InstanceID {
		t.Fatal(again, err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "marker"))
	if err != nil || string(data) != "x" {
		t.Fatal("duplicate command", string(data), err)
	}
	spec.Command = "printf different"
	if _, err := m.Start(ctx, spec); err == nil {
		t.Fatal("identity collision accepted")
	}
	reopened, err := NewManager(m.root, "/bin/sh", 4, 65536)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Inspect(ctx, "instant")
	if err != nil || saved.Status != "exited" || saved.Identity != started.Identity {
		t.Fatal(saved, err)
	}
}

func TestWaitAndClientCancellationDoNotKillPipe(t *testing.T) {
	m := manager(t, 65536)
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{ID: "long", Cwd: t.TempDir(), Command: "sleep 0.2; printf survived"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	cancel()
	state, err := m.Wait(context.Background(), spec.ID, 0)
	if err != nil || state.Status != "running" {
		t.Fatal(state, err)
	}
	out, err := m.Read(context.Background(), spec.ID, "stdout", 0, 1024)
	if err != nil || len(out.Data) != 0 || out.Status != "running" {
		t.Fatal(out, err)
	}
	state, err = m.Wait(context.Background(), spec.ID, 3*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	out, err = m.Read(context.Background(), spec.ID, "stdout", 0, 1024)
	if err != nil || string(out.Data) != "survived" {
		t.Fatal(out, err)
	}
}

func TestOutputBudgetAndCursorGap(t *testing.T) {
	m := manager(t, 1024)
	ctx := context.Background()
	spec := Spec{ID: "burst", Cwd: t.TempDir(), Command: "head -c 4096 /dev/zero"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, spec.ID, 3*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	out, err := m.Read(ctx, spec.ID, "stdout", 0, 512)
	if err != nil || len(out.Data) != 512 || out.Gap != nil || out.Next != 512 {
		t.Fatal(out, err)
	}
	out, err = m.Read(ctx, spec.ID, "stdout", out.Next, 512)
	if err != nil || len(out.Data) != 512 || out.Gap == nil || out.Gap.From != 1024 || out.Gap.To != 4096 || out.Next != 4096 {
		t.Fatal(out, err)
	}
	if _, err := m.Read(ctx, spec.ID, "stdout", 5000, 512); err == nil {
		t.Fatal("future cursor accepted")
	}
	st, err := os.Stat(filepath.Join(m.root, spec.ID, "stdout"))
	if err != nil || st.Size() != 1024 {
		t.Fatal(st, err)
	}
}

func TestRecoveryDoesNotTrustReusedPID(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	spec := Spec{ID: "identity", Cwd: t.TempDir(), Command: "true"}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(ctx, spec.ID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	e, _ := m.get(spec.ID)
	e.mu.Lock()
	e.state.Status = "running"
	e.state.PID = os.Getpid()
	e.state.Identity = "different-boot-or-start"
	e.state.ExitCode = nil
	err := e.save()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, "/bin/sh", 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.Inspect(ctx, spec.ID)
	if err != nil || state.Status != "lost" || state.StdinOpen {
		t.Fatal(state, err)
	}
	if identity(os.Getpid()) == "" {
		t.Fatal("kernel identity missing")
	}
}

func TestPTYIsNotSilentlyPiped(t *testing.T) {
	m := manager(t, 4096)
	m.tmux = ""
	if _, err := m.Start(context.Background(), Spec{ID: "pty", PTY: true, Cwd: t.TempDir(), Command: "tty"}); err == nil {
		t.Fatal("missing tmux silently succeeded")
	}
}
