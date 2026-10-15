package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/workspace"
)

func worktreeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Zende Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(data))
	}
	return strings.TrimSpace(string(data))
}

func worktreeParent(t *testing.T, c *Core) protocol.Accepted {
	t.Helper()
	dir := t.TempDir()
	worktreeTestGit(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, dir, "add", "tracked")
	worktreeTestGit(t, dir, "commit", "-m", "base")
	p := protocol.CreateAgent{Cwd: dir}
	req, _ := protocol.NewRequest("agent.create", protocol.NewID("parent"), p)
	a, err := c.Store.Create(context.Background(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWorktreeSpawnRunsAndRetainsDirectoryAfterCompletion(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, fixture.New(fixture.Step{Events: fixture.Answer("final", "finished")}))
	parent := worktreeParent(t, c)
	selection, _ := c.Store.Selection(ctx, parent.AgentID)
	path := filepath.Join(t.TempDir(), "child")
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "isolated task", Workspace: "worktree", WorktreePath: path, Context: "none"}
	req, _ := protocol.NewRequest("agent.spawn", "isolated", p)
	accepted, err := c.spawnAgent(ctx, req, p)
	if err != nil || accepted.Workspace == nil || accepted.Workspace.Path != path {
		t.Fatal(accepted, err)
	}
	if err := os.WriteFile(filepath.Join(path, "child-result"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	runCore(t, c)
	snap := waitTask(t, c, accepted, "completed")
	if snap.Agent.Cwd != path || snap.Agent.ParentID != parent.AgentID {
		t.Fatal(snap)
	}
	if got, err := os.ReadFile(filepath.Join(path, "child-result")); err != nil || string(got) != "keep" {
		t.Fatal("result removed", err)
	}
	if _, err := os.Stat(filepath.Join(selection.Choice.Cwd, "child-result")); !os.IsNotExist(err) {
		t.Fatal("result merged", err)
	}
	if worktreeTestGit(t, selection.Choice.Cwd, "rev-parse", "HEAD") != accepted.Workspace.Commit {
		t.Fatal("parent branch changed")
	}
	again, err := c.spawnAgent(ctx, req, p)
	if err != nil || !reflect.DeepEqual(accepted, again) {
		t.Fatal("retry recreated workspace/Agent", again, err)
	}
}

func TestWorktreeSpawnRecoversOnlyOwnedCompleteCheckout(t *testing.T) {
	for _, state := range []string{"prepared", "creating-clean", "creating-dirty", "creating-absent", "ready-dirty"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			c := loopCore(t, nil)
			parent := worktreeParent(t, c)
			settings, _ := c.Store.Selection(ctx, parent.AgentID)
			p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "child", Workspace: "worktree", WorktreePath: filepath.Join(t.TempDir(), "child")}
			req, _ := protocol.NewRequest("agent.spawn", "recover", p)
			plan, err := workspace.PrepareWorktree(ctx, settings.Choice.Cwd, p.WorktreePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Store.PrepareWorktree(ctx, req, p, plan, 0, 8, 4); err != nil {
				t.Fatal(err)
			}
			if state != "prepared" {
				if _, err := c.Store.TransitionWorktree(ctx, req, "prepared", "creating", nil); err != nil {
					t.Fatal(err)
				}
				if state != "creating-absent" {
					lock, err := workspace.LockWorktree(c.Store.WorktreeLockPath(req.OperationID))
					if err != nil {
						t.Fatal(err)
					}
					err = workspace.CreateWorktree(ctx, plan, lock)
					lock.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasSuffix(state, "dirty") {
					if err := os.WriteFile(filepath.Join(plan.Path, "tracked"), []byte("later edit"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if state == "ready-dirty" {
					if _, err := c.Store.TransitionWorktree(ctx, req, "creating", "ready", nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			// A fresh Store/Core observes only durable state, not former local vars.
			dbPath := filepath.Dir(filepath.Dir(c.Store.WorktreeLockPath(req.OperationID)))
			reopened, err := store.Open(filepath.Join(dbPath, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			next := New(reopened, c.Config)
			accepted, err := next.spawnAgent(ctx, req, p)
			if state == "creating-dirty" || state == "creating-absent" {
				if !isFailure(err, "worktree_unknown") || accepted.AgentID != "" {
					t.Fatal(accepted, err)
				}
				if state == "creating-absent" {
					if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
						t.Fatal("unknown operation replayed")
					}
				}
				query, _ := protocol.NewRequest("operation.inspect", "", map[string]string{"operation_id": req.OperationID})
				value, err := next.Handle(ctx, query)
				if err != nil {
					t.Fatal(err)
				}
				operation, ok := value.(*protocol.WorktreeOperation)
				if !ok || operation.State != "unknown" || operation.Failure == nil {
					t.Fatal("unverifiable checkout did not persist unknown", value)
				}
				data, _ := json.Marshal(value)
				if !strings.Contains(string(data), plan.Path) {
					t.Fatal("unaccepted creation cannot be inspected", string(data))
				}
				return
			}
			if err != nil || accepted.Workspace == nil || *accepted.Workspace != plan {
				t.Fatal(accepted, err)
			}
		})
	}
}
