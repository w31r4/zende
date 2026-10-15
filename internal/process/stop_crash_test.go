package process

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

// Run this package's real runner service in a separate OS process. Its abrupt
// death must not erase a frozen scope or accidentally restart pending commands.
func TestScopeStopRunnerCrashHelper(t *testing.T) {
	root := os.Getenv("ZENDE_TEST_STOP_RUNNER_ROOT")
	if root == "" {
		return
	}
	m, err := NewManager(filepath.Join(root, "processes"), "/bin/sh", 8, 8192)
	if err != nil {
		t.Fatal(err)
	}
	server, err := protocol.Listen(filepath.Join(root, "r.sock"), Service{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScopeStopSurvivesRealRunnerSIGKILL(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := Remote{Client: protocol.Client{Socket: filepath.Join(root, "r.sock")}}
	start := func() func() {
		log, err := os.CreateTemp(root, "runner-log-")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestScopeStopRunnerCrashHelper$")
		cmd.Env = append(os.Environ(), "ZENDE_TEST_STOP_RUNNER_ROOT="+root)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				_ = log.Close()
			}
		}
		t.Cleanup(stop)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			req, _ := protocol.NewRequest("runner.status", "", struct{}{})
			var status struct {
				PID int `json:"pid"`
			}
			if err := r.Client.Call(ctx, req, &status); err == nil && status.PID == cmd.Process.Pid {
				return stop
			}
			time.Sleep(10 * time.Millisecond)
		}
		stop()
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("runner did not listen: %s", data)
		return stop
	}
	stop := start()
	// Cleanup addresses only this test's private socket, never the user's tmux.
	t.Cleanup(func() { _, _ = tmuxCommand("tmux", terminalSocket(filepath.Join(root, "processes")), "kill-server") })
	for _, pty := range []bool{false, true} {
		id := "process-" + strconv.FormatBool(pty)
		spec := Spec{ID: id, AgentID: "agent", TaskID: "task", Cwd: root, PTY: pty, Command: "printf ready; sleep 60"}
		state, err := r.Start(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if identity(state.PID) == state.Identity {
				_ = syscall.Kill(-state.PID, syscall.SIGKILL)
			}
		})
		stream := "stdout"
		if pty {
			stream = "pty"
		}
		ready := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			out, err := r.Read(ctx, id, stream, 0, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out.Data), "ready") {
				ready = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !ready {
			t.Fatal("test command never became ready")
		}
	}
	scope := StopScope{Agents: []string{"agent"}, Pending: []StopDispatch{{ID: "late", AgentID: "agent", TaskID: "task"}}}
	if r, err := r.FreezeStop(ctx, "crash-stop", scope); err != nil || len(r.Processes) != 3 {
		t.Fatal(r, err)
	}
	stop() // SIGKILL the real runner after its durable freeze receipt.
	start()
	if saved, err := r.InspectStop(ctx, "crash-stop"); err != nil || saved.State != "frozen" {
		t.Fatal(saved, err)
	}
	_, err := r.Start(ctx, Spec{ID: "late", AgentID: "agent", TaskID: "task", Cwd: root, Command: "touch forbidden"})
	requireStopError(t, err, "tree_stopping")
	finishStop(t, r, "crash-stop")
	if _, err := r.ReleaseStop(ctx, "crash-stop"); err != nil {
		t.Fatal(err)
	}
	_, err = r.Start(ctx, Spec{ID: "late", AgentID: "agent", TaskID: "task", Cwd: root, Command: "touch forbidden"})
	requireStopError(t, err, "process_cancelled")
	if state, err := r.Wait(ctx, "process-true", 5*time.Second); err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	for _, p := range []struct{ id, stream string }{{"process-false", "stdout"}, {"process-true", "pty"}} {
		if out, err := r.Read(ctx, p.id, p.stream, 0, 8192); err != nil || !strings.Contains(string(out.Data), "ready") {
			t.Fatal(out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("late command executed after restart", err)
	}
}
