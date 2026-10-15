package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/workspace"
)

// Real Git holds an inherited lock while its checkout filter is gated. Killing
// only this test's core must neither cancel checkout nor let a retry adopt it.
func TestBinaryWorktreeRecoversAfterCoreSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("real process integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/zende").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	runDir, err := os.MkdirTemp("", "zende-wt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "XDG_") && !strings.HasPrefix(item, "ZENDE_") && !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	env = append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+runDir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--profile", "test"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	must := func(args ...string) []byte {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatal(args, err, string(out))
		}
		return out
	}
	wait := func(label string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal(label)
	}
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(dir, "source")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		opts := []string{"-c", "user.name=Zende Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}
		cmd := exec.CommandContext(ctx, "git", append(opts, args...)...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	write(filepath.Join(repo, "tracked"), "committed\n", 0600)
	write(filepath.Join(repo, ".gitattributes"), "tracked filter=fixture\n", 0600)
	git("add", ".")
	git("commit", "-m", "base")
	head := git("rev-parse", "HEAD")
	gate, marker := filepath.Join(dir, "release"), filepath.Join(dir, "checkout-starts")
	filter := filepath.Join(dir, "gated-filter")
	write(filter, "#!/bin/sh\nprintf 'start\\n' >> "+quoteCommandPart(marker)+"\nwhile [ ! -f "+quoteCommandPart(gate)+" ]; do sleep 0.02; done\ncat\n", 0700)
	git("config", "filter.fixture.smudge", quoteCommandPart(filter))
	git("config", "filter.fixture.clean", "cat")
	git("config", "filter.fixture.required", "true")

	pids := []int{}
	t.Cleanup(func() {
		for _, pid := range pids {
			exe := filepath.Join("/proc", fmt.Sprint(pid), "exe")
			if current, _ := os.Readlink(exe); current != binary {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if current, _ := os.Readlink(exe); current != binary {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if current, _ := os.Readlink(exe); current == binary {
				t.Errorf("test core %d did not stop", pid)
			}
		}
	})
	start := func() int {
		t.Helper()
		var state struct {
			PID int `json:"pid"`
		}
		if err := json.Unmarshal(must("core", "start"), &state); err != nil || state.PID <= 0 {
			t.Fatal(state, err)
		}
		pids = append(pids, state.PID)
		return state.PID
	}
	pid := start()
	var parent protocol.Accepted
	if err := json.Unmarshal(must("agent", "create", "--cwd", repo), &parent); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "child")
	args := []string{"--request-id", "worktree-request", "agent", "spawn", "--parent", parent.AgentID, "--context", "none", "--workspace", "worktree", "--worktree-path", target, "child task"}
	// Release filters even when an assertion fails, and wait for the checkout
	// before removing the test directories.
	var plan protocol.Worktree
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("release"), 0600)
		if plan.Path == "" {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := workspace.VerifyWorktree(ctx, plan, true)
			cancel()
			if err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("test checkout did not finish")
	})
	first := make(chan error, 1)
	go func() { _, err := run(args...); first <- err }()
	wait("checkout filter did not start", func() bool { _, err := os.Stat(marker); return err == nil })
	var pending protocol.WorktreeOperation
	if err := json.Unmarshal(must("operation", "inspect", "worktree-request"), &pending); err != nil || pending.State != "creating" || pending.Workspace.Commit != head {
		t.Fatal(pending, err)
	}
	plan = pending.Workspace
	if plan.Source != repo || plan.Path != target {
		t.Fatal("source came from CLI cwd instead of parent", plan)
	}
	if current, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe")); current != binary {
		t.Fatal("test core identity changed")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait("test core did not exit", func() bool {
		exe, _ := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe"))
		return exe != binary
	})
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("unfinished checkout accepted")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first client did not exit")
	}
	start()
	out, err := run(args...)
	if err == nil || !strings.Contains(string(out), "worktree_busy") {
		t.Fatal("new core bypassed surviving Git lock", err, string(out))
	}
	var agents []protocol.Agent
	if err := json.Unmarshal(must("agent", "list"), &agents); err != nil || len(agents) != 1 {
		t.Fatal("child published before checkout", agents, err)
	}
	write(gate, "release", 0600)
	var accepted protocol.Accepted
	wait("original checkout not recovered", func() bool {
		out, err = run(args...)
		if err != nil {
			if !strings.Contains(string(out), "worktree_busy") {
				t.Fatal(err, string(out))
			}
			return false
		}
		if err := json.Unmarshal(out, &accepted); err != nil {
			t.Fatal(err, string(out))
		}
		return true
	})
	if accepted.Workspace == nil || *accepted.Workspace != plan || accepted.TaskID == "" {
		t.Fatal(accepted)
	}
	var retried protocol.Accepted
	if err := json.Unmarshal(must(args...), &retried); err != nil || !reflect.DeepEqual(retried, accepted) {
		t.Fatal("retry changed acceptance", retried, err)
	}
	if err := json.Unmarshal(must("agent", "list"), &agents); err != nil || len(agents) != 2 {
		t.Fatal("duplicate or missing child", agents, err)
	}
	count, err := os.ReadFile(marker)
	if err != nil || string(count) != "start\n" {
		t.Fatal("checkout was replayed", string(count), err)
	}
	if git("rev-parse", "HEAD") != head {
		t.Fatal("parent HEAD changed")
	}
	t.Log("core SIGKILL during real Git checkout: original worktree retained, one checkout, one child, retry receipt unchanged")
}
