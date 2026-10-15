package process

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRecoveryQuarantinesOnlyDamagedRecord(t *testing.T) {
	for _, damage := range []string{"missing", "json", "version", "identity", "stream", "fifo"} {
		t.Run(damage, func(t *testing.T) {
			m := manager(t, 4096)
			ctx := context.Background()
			good := completedOutput(t, m, "good", "", "printf intact")
			dir := filepath.Join(m.root, "damaged")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s := good
			s.ID = "damaged"
			s.Command = "printf x >> marker"
			switch damage {
			case "version":
				s.SchemaVersion = 999
			case "identity":
				s.ID = "another-id"
			case "stream":
				s.Output = map[string]StreamState{"../good/stdout": {Stored: 6, Total: 6}}
			}
			data, _ := json.Marshal(s)
			if damage == "json" {
				data = []byte("{broken")
			}
			if damage == "fifo" {
				if err := syscall.Mkfifo(filepath.Join(dir, "state.json"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if damage != "missing" {
				if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "pty"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewManager(m.root, m.shell, 4, 4096)
			if err != nil {
				t.Fatal(err)
			}
			state, err := reopened.Inspect(ctx, "damaged")
			if err != nil || state.Status != "unknown" || !strings.Contains(state.Failure, "preserved") {
				t.Fatal(state, err)
			}
			s.ID = "damaged"
			if _, err := reopened.Start(ctx, s.Spec); err == nil {
				t.Fatal("reserved ID dispatched")
			}
			if _, err := reopened.Signal(ctx, "must-not-signal", SignalParams{ID: "damaged", Signal: "KILL"}); err == nil {
				t.Fatal("invalid identity was signalled")
			}
			if _, err := reopened.Rotate(ctx, "must-not-delete", RotateParams{}); err == nil {
				t.Fatal("quarantined bytes were not protected")
			}
			if _, err := reopened.Read(ctx, "damaged", "pty", 0, 100); err == nil {
				t.Fatal("unverifiable range accepted")
			}
			out, err := reopened.Read(ctx, "good", "stdout", 0, 100)
			if err != nil || string(out.Data) != "intact" {
				t.Fatal(out, err)
			}
			if damage == "fifo" {
				st, err := os.Lstat(filepath.Join(dir, "state.json"))
				if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
					t.Fatal("original file type changed", err)
				}
				return
			}
			after, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if damage == "missing" {
				if !os.IsNotExist(err) {
					t.Fatal("missing metadata replaced", err)
				}
			} else if err != nil || !bytes.Equal(after, data) {
				t.Fatal("original metadata overwritten", err)
			}
			completedOutput(t, reopened, "new-work", "", "true")
		})
	}
}

func TestRecoveryDispatchWindowsNeverRerun(t *testing.T) {
	for _, window := range []string{"pipe-intent", "pipe-effect-before-identity", "pty-intent"} {
		t.Run(window, func(t *testing.T) {
			m := manager(t, 4096)
			cwd := t.TempDir()
			dir := filepath.Join(m.root, "interrupted")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s := State{Spec: Spec{ID: "interrupted", Cwd: cwd, Command: "printf x >> marker"}, SchemaVersion: 1, Status: "starting", Output: map[string]StreamState{"stdout": {}, "stderr": {}}}
			if window == "pty-intent" {
				s.PTY, s.Cols, s.Rows = true, 80, 24
				s.Output = map[string]StreamState{"pty": {}}
			}
			if window == "pipe-effect-before-identity" {
				if err := os.WriteFile(filepath.Join(cwd, "marker"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("persisted"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveJSON(filepath.Join(dir, "state.json"), s); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewManager(m.root, m.shell, 4, 4096)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			state, err := reopened.Start(ctx, s.Spec)
			if err != nil || state.Status != "unknown" || state.ExitCode != nil || state.StdinOpen {
				t.Fatal(state, err)
			}
			marker, err := os.ReadFile(filepath.Join(cwd, "marker"))
			if window == "pipe-effect-before-identity" {
				if err != nil || string(marker) != "x" {
					t.Fatal("effect repeated", string(marker), err)
				}
				out, err := reopened.Read(ctx, s.ID, "stdout", 0, 100)
				if err != nil || string(out.Data) != "persisted" {
					t.Fatal(out, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("intent was replayed", string(marker), err)
			}
		})
	}
}

func TestPipeRecoveryPreservesMissingRange(t *testing.T) {
	m := manager(t, 4096)
	s := completedOutput(t, m, "truncated", "", "printf saved")
	s.Status, s.ExitCode, s.Identity = "running", nil, "old-boot"
	path := filepath.Join(m.root, s.ID)
	if err := saveJSON(filepath.Join(path, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(path, "stdout"), 2); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.Inspect(context.Background(), s.ID)
	if err != nil || state.Status != "lost" || state.Output["stdout"].Stored != 5 || !strings.Contains(state.Failure, "incomplete") {
		t.Fatal(state, err)
	}
	if _, err := reopened.Read(context.Background(), s.ID, "stdout", 0, 100); err == nil {
		t.Fatal("missing range hidden on recovery")
	}
}

func TestPTYCaptureAndServerLoss(t *testing.T) {
	for _, failure := range []string{"collector", "server", "counter"} {
		t.Run(failure, func(t *testing.T) {
			m := tmuxManager(t, 4096)
			ctx := context.Background()
			cwd := t.TempDir()
			spec := Spec{ID: "lost-capture", Cwd: cwd, PTY: true, Command: "printf x >> marker; printf saved; while [ ! -f release ]; do sleep 0.02; done"}
			s, err := m.Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			waitOutput(t, m, s.ID, "pty", "saved")
			path := filepath.Join(m.root, s.ID, "capture.json")
			var capture captureRecord
			if err := readJSON(path, &capture); err != nil {
				t.Fatal(err)
			}
			if failure == "server" {
				if _, err := tmuxCommand(m.tmux, m.socket, "kill-server"); err != nil {
					t.Fatal(err)
				}
			} else {
				if capture.Identity == "" || identity(capture.PID) != capture.Identity {
					t.Fatal("test collector identity changed")
				}
				if err := syscall.Kill(capture.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && identity(capture.PID) == capture.Identity; {
					time.Sleep(10 * time.Millisecond)
				}
				if failure == "counter" {
					capture.Output.Stored = -1
					if err := saveJSON(path, capture); err != nil {
						t.Fatal(err)
					}
				}
			}
			reopened, err := NewManager(m.root, m.shell, 4, 4096)
			if err != nil {
				t.Fatal(err)
			}
			state, err := reopened.Start(ctx, spec)
			if err != nil || state.Status != "unknown" || state.ExitCode != nil || state.StdinOpen {
				t.Fatal(state, err)
			}
			marker, err := os.ReadFile(filepath.Join(cwd, "marker"))
			if err != nil || string(marker) != "x" {
				t.Fatal("lost process replayed", string(marker), err)
			}
			if failure != "counter" {
				out, err := reopened.Read(ctx, s.ID, "pty", 0, 100)
				if err != nil || string(out.Data) != "saved" {
					t.Fatal(out, err)
				}
			}
		})
	}
}
