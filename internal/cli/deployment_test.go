package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

// This test uses the real CLI executable and detached cores, with isolated XDG
// directories. Every process killed here was started by this test's profile.
func TestDetachedCoreBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("real process integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/zende")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("", "zende-core-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	terminate := func(pid int) {
		// Validate the executable, not a reusable PID alone.
		path := filepath.Join("/proc", fmt.Sprint(pid), "exe")
		if current, _ := os.Readlink(path); current != binary {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if current, _ := os.Readlink(path); current != binary {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("test core %d did not stop", pid)
	}
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "XDG_") && !strings.HasPrefix(item, "ZENDE_") {
			env = append(env, item)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir)
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	type status struct {
		Instance string `json:"instance_id"`
		PID      int    `json:"pid"`
	}
	results := make(chan status, 2)
	failures := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := run("--profile", "isolated", "core", "start")
			if err != nil {
				failures <- string(out)
				return
			}
			var s status
			if err := json.Unmarshal(out, &s); err != nil {
				failures <- string(out)
				return
			}
			results <- s
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	var states []status
	for s := range results {
		states = append(states, s)
	}
	// Clean up even when one of the two concurrent clients failed.
	for _, s := range states {
		if s.PID > 0 {
			pid := s.PID
			t.Cleanup(func() { terminate(pid) })
		}
	}
	for failure := range failures {
		t.Error(failure)
	}
	if len(states) != 2 {
		t.FailNow()
	}
	if states[0] != states[1] || states[0].PID == 0 || states[0].Instance == "" {
		t.Fatal("concurrent starts disagree", states)
	}
	out, err := run("--profile", "isolated", "--request-id", "durable-request", "agent", "create", "persist after clients exit")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var accepted protocol.Accepted
	if err := json.Unmarshal(out, &accepted); err != nil {
		t.Fatal(err)
	}
	// Both launch clients have exited; the independently owned core still serves.
	out, err = run("--profile", "isolated", "agent", "inspect", accepted.AgentID)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(out, &snap); err != nil || len(snap.Tasks) != 1 {
		t.Fatal(err, string(out))
	}
	// Runner is another OS process, not a goroutine in core.
	out, err = run("--profile", "isolated", "runner", "start")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var runner status
	if err := json.Unmarshal(out, &runner); err != nil || runner.PID == 0 || runner.PID == states[0].PID {
		t.Fatal("runner not independent", string(out), err)
	}
	t.Cleanup(func() { terminate(runner.PID) })
	gate := filepath.Join(dir, "release")
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("ready"), 0600)
		_, _ = run("--profile", "isolated", "process", "wait", "persistent-pipe", "--wait-ms", "3000")
	})
	out, err = run("--profile", "isolated", "--request-id", "persistent-pipe", "process", "exec", "while [ ! -f release ]; do sleep 0.01; done; printf survived-core-crash")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var original process.State
	if err := json.Unmarshal(out, &original); err != nil || original.Status != "running" {
		t.Fatal(string(out), err)
	}
	// Abruptly kill only this test-owned core; runner and pipe must survive.
	if exe, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(states[0].PID), "exe")); exe != binary {
		t.Fatal("test process identity changed")
	}
	if err := syscall.Kill(states[0].PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = run("--profile", "isolated", "core", "status"); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("core did not exit")
	}
	out, err = run("--profile", "isolated", "process", "inspect", original.ID)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var retained process.State
	if err := json.Unmarshal(out, &retained); err != nil || retained.Status != "running" || retained.PID != original.PID || retained.Identity != original.Identity || retained.InstanceID != original.InstanceID {
		t.Fatal("pipe lost during core crash", string(out), err)
	}
	out, err = run("--profile", "isolated", "core", "start")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var restarted status
	if err := json.Unmarshal(out, &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.PID > 0 {
		t.Cleanup(func() { terminate(restarted.PID) })
	}
	if restarted.Instance == states[0].Instance {
		t.Fatal("instance identity reused")
	}
	out, err = run("--profile", "isolated", "config", "inspect")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var inspected struct {
		Paths resource.Paths `json:"paths"`
	}
	if err := json.Unmarshal(out, &inspected); err != nil {
		t.Fatal(err)
	}
	request, _ := protocol.NewRequest("process.inspect", "", process.Target{ID: original.ID})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = (protocol.Client{Socket: inspected.Paths.Socket}).Call(ctx, request, &retained)
	cancel()
	if err != nil || retained.Identity != original.Identity || retained.InstanceID != original.InstanceID || retained.Status != "running" {
		t.Fatal("new core did not reconnect to original runner process", retained, err)
	}
	out, err = run("--profile", "isolated", "--request-id", "durable-request", "agent", "create", "persist after clients exit")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var replay protocol.Accepted
	if err := json.Unmarshal(out, &replay); err != nil || replay != accepted {
		t.Fatal("restart duplicated acceptance", string(out), err)
	}
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = run("--profile", "isolated", "process", "wait", original.ID, "--wait-ms", "3000")
	if err != nil {
		t.Fatal(err, string(out))
	}
	var finished process.State
	if err := json.Unmarshal(out, &finished); err != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatal(string(out), err)
	}
	out, err = run("--profile", "isolated", "process", "output", original.ID)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var output process.Output
	if err := json.Unmarshal(out, &output); err != nil || string(output.Data) != "survived-core-crash" {
		t.Fatal(string(out), err)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("PTY crash recovery requires real tmux")
	}
	// Unlike PTY capture, pipe descriptors belong to the runner. Keep a pipe
	// command alive across the same runner crash and verify conservative recovery.
	pipeCommand := "printf x >> pipe-marker; printf saved-prefix; while [ ! -f pipe-release ]; do sleep 0.02; done"
	out, err = run("--profile", "isolated", "--request-id", "orphan-pipe", "process", "exec", pipeCommand)
	if err != nil {
		t.Fatal(string(out), err)
	}
	var orphan process.State
	if err := json.Unmarshal(out, &orphan); err != nil || orphan.Status != "running" {
		t.Fatal(string(out), err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(dir, "pipe-release"), nil, 0600) })
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, err = run("--profile", "isolated", "process", "output", orphan.ID)
		if err == nil && json.Unmarshal(out, &output) == nil && string(output.Data) == "saved-prefix" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || string(output.Data) != "saved-prefix" {
		t.Fatal("pipe prefix not captured", string(out), err)
	}
	out, err = run("--profile", "isolated", "--request-id", "persistent-pty", "process", "exec", "--pty", "--cols", "93", "--rows", "32",
		"test -t 0 && test -t 1 && test -t 2 || exit 98; printf before; while [ ! -f pty-release ]; do sleep 0.01; done; printf after; exit 9")
	if err != nil {
		t.Fatal(string(out), err)
	}
	var terminal process.State
	if err := json.Unmarshal(out, &terminal); err != nil || terminal.Terminal == nil {
		t.Fatal(string(out), err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", terminal.Terminal.Socket, "kill-server").Run()
	})
	if exe, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(runner.PID), "exe")); exe != binary {
		t.Fatal("runner identity changed")
	}
	if err := syscall.Kill(runner.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, err = run("--profile", "isolated", "runner", "status"); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("runner did not exit")
	}
	out, err = run("--profile", "isolated", "runner", "start")
	if err != nil {
		t.Fatal(string(out), err)
	}
	var newRunner status
	if err := json.Unmarshal(out, &newRunner); err != nil || newRunner.PID == 0 {
		t.Fatal(string(out), err)
	}
	t.Cleanup(func() { terminate(newRunner.PID) })
	if newRunner.Instance == runner.Instance {
		t.Fatal("runner instance was reused")
	}
	out, err = run("--profile", "isolated", "--request-id", orphan.ID, "process", "exec", pipeCommand)
	if err != nil {
		t.Fatal(string(out), err)
	}
	var orphanRecovered process.State
	if err := json.Unmarshal(out, &orphanRecovered); err != nil || orphanRecovered.Status != "unknown" || orphanRecovered.PID != orphan.PID || orphanRecovered.Identity != orphan.Identity || orphanRecovered.StdinOpen || orphanRecovered.ExitCode != nil {
		t.Fatal("pipe descriptors or exit fabricated", string(out), err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "pipe-marker"))
	if err != nil || string(marker) != "x" {
		t.Fatal("orphan pipe replayed", string(marker), err)
	}
	out, err = run("--profile", "isolated", "process", "output", orphan.ID)
	if err != nil || json.Unmarshal(out, &output) != nil || string(output.Data) != "saved-prefix" {
		t.Fatal(string(out), err)
	}
	out, err = run("--profile", "isolated", "process", "stdin", orphan.ID, "must-not-write")
	if err == nil {
		t.Fatal("lost stdin accepted", string(out))
	}
	out, err = run("--profile", "isolated", "process", "terminate", orphan.ID, "--force")
	if err != nil {
		t.Fatal("could not stop verified orphan", string(out), err)
	}
	out, err = run("--profile", "isolated", "process", "inspect", terminal.ID)
	if err != nil {
		t.Fatal(string(out), err)
	}
	var attached process.State
	if err := json.Unmarshal(out, &attached); err != nil || attached.Status != "running" || attached.Identity != terminal.Identity || attached.InstanceID != terminal.InstanceID || attached.Terminal == nil || *attached.Terminal != *terminal.Terminal {
		t.Fatal("PTY was not recovered", string(out), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pty-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = run("--profile", "isolated", "process", "wait", terminal.ID, "--wait-ms", "5000")
	if err != nil {
		t.Fatal(string(out), err)
	}
	if err := json.Unmarshal(out, &finished); err != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 9 {
		t.Fatal(string(out), err)
	}
	out, err = run("--profile", "isolated", "process", "output", terminal.ID, "--stream", "pty")
	if err != nil {
		t.Fatal(string(out), err)
	}
	if err := json.Unmarshal(out, &output); err != nil || string(output.Data) != "beforeafter" {
		t.Fatal(string(out), err)
	}
	// Restart again after the orphan is gone and the PTY completion is saved.
	terminate(newRunner.PID)
	out, err = run("--profile", "isolated", "runner", "start")
	if err != nil {
		t.Fatal(string(out), err)
	}
	var finalRunner status
	if err := json.Unmarshal(out, &finalRunner); err != nil || finalRunner.PID == 0 {
		t.Fatal(string(out), err)
	}
	t.Cleanup(func() { terminate(finalRunner.PID) })
	out, err = run("--profile", "isolated", "process", "inspect", orphan.ID)
	if err != nil || json.Unmarshal(out, &orphanRecovered) != nil || orphanRecovered.Status != "lost" || orphanRecovered.ExitCode != nil {
		t.Fatal("lost pipe was given a fabricated exit", string(out), err)
	}
	out, err = run("--profile", "isolated", "process", "inspect", terminal.ID)
	if err != nil || json.Unmarshal(out, &finished) != nil || finished.Status != "exited" || finished.ExitCode == nil || *finished.ExitCode != 9 {
		t.Fatal("completed PTY result lost on restart", string(out), err)
	}
}
