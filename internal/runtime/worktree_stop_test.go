package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/workspace"
)

func beginWorktreeStop(t *testing.T, c *Core, parent string) protocol.Request {
	t.Helper()
	req, _ := protocol.NewRequest("agent.stop", "stop-worktree", map[string]any{"agent_id": parent, "tree": true})
	if _, err := c.Store.BeginTreeStop(context.Background(), req, parent); err != nil {
		t.Fatal(err)
	}
	return req
}

func awaitWorkspace(t *testing.T, label string, fn func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(label)
}

func finishWorktreeStop(t *testing.T, c *Core, stop, id string) {
	t.Helper()
	awaitWorkspace(t, "workspace stop did not finish", func() bool {
		err := c.stopWorktree(context.Background(), stop, id)
		if err != nil && !isFailure(err, "worktree_busy") {
			t.Fatal(err)
		}
		return err == nil
	})
}

func gatedWorktree(t *testing.T, c *Core) (protocol.Request, protocol.SpawnAgent, string, string) {
	t.Helper()
	ctx := context.Background()
	parent := worktreeParent(t, c)
	choice, _ := c.Store.Selection(ctx, parent.AgentID)
	dir := t.TempDir()
	gate, marker := filepath.Join(dir, "release"), filepath.Join(dir, "checkout-starts")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	filter := filepath.Join(dir, "filter")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\nprintf 'start\\n' >> "+quote(marker)+"\nwhile [ ! -f "+quote(gate)+" ]; do sleep 0.02; done\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(choice.Choice.Cwd, ".gitattributes"), []byte("tracked filter=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, choice.Choice.Cwd, "add", ".gitattributes")
	worktreeTestGit(t, choice.Choice.Cwd, "commit", "-m", "checkout gate")
	worktreeTestGit(t, choice.Choice.Cwd, "config", "filter.fixture.smudge", quote(filter))
	worktreeTestGit(t, choice.Choice.Cwd, "config", "filter.fixture.clean", "cat")
	worktreeTestGit(t, choice.Choice.Cwd, "config", "filter.fixture.required", "true")
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "child", Workspace: "worktree", WorktreePath: filepath.Join(dir, "checkout")}
	req, _ := protocol.NewRequest("agent.spawn", "workspace", p)
	return req, p, gate, marker
}

func TestWorktreeStopTerminatesLiveCheckoutAndRetainsFiles(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	req, p, gate, marker := gatedWorktree(t, c)
	done := make(chan error, 1)
	spawnCtx, cancel := context.WithCancel(ctx)
	go func() { _, err := c.spawnAgent(spawnCtx, req, p); done <- err }()
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("release"), 0600)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("checkout handler remained live")
		}
	})
	awaitWorkspace(t, "checkout filter did not run", func() bool { _, err := os.Stat(marker); return err == nil })
	keep := filepath.Join(p.WorktreePath, "keep-untracked")
	if err := os.WriteFile(keep, []byte("user edits"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := beginWorktreeStop(t, c, p.ParentID)
	evidence, err := c.Store.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || evidence.Group == nil || evidence.Group.Identity == "" {
		t.Fatal(evidence, err)
	}
	finishWorktreeStop(t, c, stop.OperationID, req.OperationID)
	for range 2 {
		if err := c.stopWorktree(ctx, stop.OperationID, req.OperationID); err != nil {
			t.Fatal("stop retry", err)
		}
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "user edits" {
		t.Fatal("partial checkout contents removed", string(got), err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "start\n" {
		t.Fatal("checkout replayed", string(got), err)
	}
	op, err := c.Store.WorktreeOperation(ctx, req.OperationID)
	if err != nil || op.State != "cancelled" {
		t.Fatal(op, err)
	}
	if _, err := c.spawnAgent(ctx, req, p); !isFailure(err, "cancelled") {
		t.Fatal("cancelled request revived", err)
	}
	agents, err := c.Store.ListAgents(ctx)
	if err != nil || len(agents) != 1 {
		t.Fatal("unfinished checkout published child", agents, err)
	}
	tree, err := c.Store.TreeStop(ctx, stop.OperationID)
	if err != nil || tree.State != "stopping" {
		t.Fatal("worktree cancellation claimed whole-tree completion", tree, err)
	}
}

func TestWorktreeStopWaitsForLateIdentityAndInheritedLock(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	req, p, gate, marker := gatedWorktree(t, c)
	choice, _ := c.Store.Selection(ctx, p.ParentID)
	plan, err := workspace.PrepareWorktree(ctx, choice.Choice.Cwd, p.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.PrepareWorktree(ctx, req, p, plan, 0, 8, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
		t.Fatal(err)
	}
	lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(req.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	groupReady := make(chan process.Group, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		err := workspace.CreateTrackedWorktree(ctx, plan, lock, func(g process.Group) error {
			groupReady <- g
			<-release
			return c.Store.RecordWorktreeProcess(ctx, req, g)
		})
		lock.Close()
		done <- err
	}()
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(release)
		}
		_ = os.WriteFile(gate, []byte("release"), 0600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("late-identity checkout did not exit")
		}
	})
	select {
	case g := <-groupReady:
		if g.Identity == "" {
			t.Fatal(g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkout never started")
	}
	awaitWorkspace(t, "filter did not start", func() bool { _, err := os.Stat(marker); return err == nil })
	stop := beginWorktreeStop(t, c, p.ParentID)
	if err := c.stopWorktree(ctx, stop.OperationID, req.OperationID); !isFailure(err, "worktree_busy") {
		t.Fatal("Start-to-identity window treated as complete", err)
	}
	evidence, err := c.Store.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || evidence.Group != nil {
		t.Fatal(evidence, err)
	}
	close(release)
	closed = true
	finishWorktreeStop(t, c, stop.OperationID, req.OperationID)
}

func TestWorktreeStopDoesNotSignalBeforeEvidenceCommit(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	req, p, gate, marker := gatedWorktree(t, c)
	done := make(chan error, 1)
	spawnCtx, cancel := context.WithCancel(ctx)
	go func() { _, err := c.spawnAgent(spawnCtx, req, p); done <- err }()
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("release"), 0600)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("checkout handler remained live")
		}
	})
	awaitWorkspace(t, "filter did not start", func() bool { _, err := os.Stat(marker); return err == nil })
	stop := beginWorktreeStop(t, c, p.ParentID)
	evidence, err := c.Store.WorktreeStop(ctx, stop.OperationID, req.OperationID)
	if err != nil || evidence.Group == nil {
		t.Fatal(evidence, err)
	}
	path := filepath.Join(filepath.Dir(filepath.Dir(c.Store.WorktreeLockPath(req.OperationID))), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER reject_witness BEFORE INSERT ON events WHEN NEW.kind='workspace.stop_observed' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if err := c.stopWorktree(ctx, stop.OperationID, req.OperationID); err == nil || isFailure(err, "worktree_busy") {
		t.Fatal("stop did not report evidence write failure", err)
	}
	if _, ended, err := process.ObserveGroup(*evidence.Group); err != nil || ended {
		t.Fatal("signal preceded durable witnesses", ended, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_witness"); err != nil {
		t.Fatal(err)
	}
	finishWorktreeStop(t, c, stop.OperationID, req.OperationID)
}

// This helper really dies with SIGKILL; its Git/filter keep the inherited lock.
func TestWorktreeStopCoreCrashHelper(t *testing.T) {
	path := os.Getenv("ZENDE_TEST_WORKTREE_DB")
	if path == "" {
		return
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var req protocol.Request
	if err := json.Unmarshal([]byte(os.Getenv("ZENDE_TEST_WORKTREE_REQUEST")), &req); err != nil {
		t.Fatal(err)
	}
	var p protocol.SpawnAgent
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	c := New(s, resource.Effective{Config: resource.Defaults(filepath.Dir(path))})
	if _, err := c.spawnAgent(context.Background(), req, p); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeStopRecoversAfterRealCoreSIGKILL(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	req, p, gate, marker := gatedWorktree(t, c)
	path := filepath.Join(filepath.Dir(filepath.Dir(c.Store.WorktreeLockPath(req.OperationID))), "state.db")
	log, err := os.CreateTemp(t.TempDir(), "core-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	data, _ := json.Marshal(req)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorktreeStopCoreCrashHelper$")
	cmd.Env = append(os.Environ(), "ZENDE_TEST_WORKTREE_DB="+path, "ZENDE_TEST_WORKTREE_REQUEST="+string(data))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := false
	stopHelper := func() {
		if !dead {
			dead = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stopHelper)
	t.Cleanup(func() {
		_ = os.WriteFile(gate, []byte("release"), 0600)
		awaitWorkspace(t, "test Git retained lock after cleanup", func() bool {
			lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(req.OperationID))
			if err != nil {
				return false
			}
			lock.Close()
			return true
		})
	})
	awaitWorkspace(t, "helper checkout did not start", func() bool { _, err := os.Stat(marker); return err == nil })
	stop := beginWorktreeStop(t, c, p.ParentID)
	awaitWorkspace(t, "helper did not save process identity", func() bool {
		evidence, err := c.Store.WorktreeStop(ctx, stop.OperationID, req.OperationID)
		return err == nil && evidence.Group != nil && evidence.Group.Identity != ""
	})
	stopHelper()
	if lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(req.OperationID)); err == nil {
		lock.Close()
		t.Fatal("core death unexpectedly ended checkout")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next := New(reopened, c.Config)
	finishWorktreeStop(t, next, stop.OperationID, req.OperationID)
	if _, err := next.spawnAgent(ctx, req, p); !isFailure(err, "cancelled") {
		t.Fatal("new core replayed checkout", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "start\n" {
		t.Fatal("checkout replayed", string(got), err)
	}
	if _, err := os.Stat(filepath.Join(p.WorktreePath, ".git")); err != nil {
		t.Fatal("original partial workspace removed", err)
	}
	t.Log("SIGKILL core, reopen store, terminate original Git/filter scope, retain checkout, reject replay")
}
