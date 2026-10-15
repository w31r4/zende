package workspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func seedWorktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testGit(t, dir, "init", "-b", "main")
	writeFixture(t, filepath.Join(dir, "file"), "committed\n", 0644)
	testGit(t, dir, "add", "file")
	testGit(t, dir, "commit", "-m", "base")
	return dir
}

func TestWorktreeRealCreationPreservesSourceAndResults(t *testing.T) {
	ctx := context.Background()
	dir := seedWorktreeRepo(t)
	beforeHead := testGit(t, dir, "rev-parse", "HEAD")
	beforeRefs := testGit(t, dir, "for-each-ref")
	writeFixture(t, filepath.Join(dir, "file"), "uncommitted parent\n", 0644)
	writeFixture(t, filepath.Join(dir, "parent-only"), "untracked", 0644)
	index, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	target := filepath.Join(t.TempDir(), "中文 child\nwith spaces")
	plan, err := PrepareWorktree(ctx, dir, target)
	if err != nil || plan.Commit != beforeHead || plan.Path != target {
		t.Fatal(plan, err)
	}
	lock, err := LockWorktree(filepath.Join(t.TempDir(), "creation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := CreateWorktree(ctx, plan, lock); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWorktree(ctx, plan, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "file"))
	if err != nil || string(got) != "committed\n" {
		t.Fatal(string(got), err)
	}
	if _, err := os.Stat(filepath.Join(target, "parent-only")); !os.IsNotExist(err) {
		t.Fatal("untracked parent file copied", err)
	}
	if err := CreateWorktree(ctx, plan, lock); err == nil {
		t.Fatal("created twice")
	}
	writeFixture(t, filepath.Join(target, "file"), "child work\n", 0644)
	if err := VerifyWorktree(ctx, plan, false); err != nil {
		t.Fatal("saved receipt must allow edits", err)
	}
	if err := VerifyWorktree(ctx, plan, true); err == nil {
		t.Fatal("dirty checkout treated as proof of unknown creation")
	}
	if testGit(t, dir, "rev-parse", "HEAD") != beforeHead || testGit(t, dir, "for-each-ref") != beforeRefs {
		t.Fatal("source refs changed")
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if string(index) != string(after) {
		t.Fatal("source index changed")
	}
	parent, _ := os.ReadFile(filepath.Join(dir, "file"))
	if string(parent) != "uncommitted parent\n" {
		t.Fatal("source work changed")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("worktree was removed", err)
	}
}

func TestWorktreeRejectsInvalidOrReplacedDestinations(t *testing.T) {
	ctx := context.Background()
	dir := seedWorktreeRepo(t)
	for _, variant := range []string{"non-git", "unborn", "exists", "symlink", "admin", "parent-missing", "appeared", "parent-replaced", "owner-changed", "git-link-replaced"} {
		t.Run(variant, func(t *testing.T) {
			source, target := dir, filepath.Join(t.TempDir(), "new")
			switch variant {
			case "non-git":
				source = t.TempDir()
			case "unborn":
				source = t.TempDir()
				testGit(t, source, "init", "-b", "main")
			case "exists":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), target); err != nil {
					t.Fatal(err)
				}
			case "admin":
				target = filepath.Join(dir, ".git", "child")
			case "parent-missing":
				target = filepath.Join(target, "deeper")
			}
			plan, err := PrepareWorktree(ctx, source, target)
			if variant != "appeared" && variant != "parent-replaced" && variant != "owner-changed" && variant != "git-link-replaced" {
				if err == nil {
					t.Fatal("invalid preparation accepted", plan)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lock, err := LockWorktree(filepath.Join(t.TempDir(), "lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if variant == "owner-changed" || variant == "git-link-replaced" {
				if err := CreateWorktree(ctx, plan, lock); err != nil {
					t.Fatal(err)
				}
				if variant == "owner-changed" {
					plan.Owner = "another-operation"
				} else {
					other := filepath.Join(t.TempDir(), "other")
					testGit(t, dir, "worktree", "add", "--detach", other, "HEAD")
					link, err := os.ReadFile(filepath.Join(other, ".git"))
					if err != nil {
						t.Fatal(err)
					}
					writeFixture(t, filepath.Join(plan.Path, ".git"), string(link), 0600)
				}
				if err := VerifyWorktree(ctx, plan, false); err == nil {
					t.Fatal("unowned worktree adopted")
				}
				return
			}
			if variant == "appeared" {
				writeFixture(t, target, "preserve", 0600)
			} else {
				parent := filepath.Dir(target)
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(parent); _ = os.Rename(parent+"-original", parent) })
				if err := os.Symlink(t.TempDir(), parent); err != nil {
					t.Fatal(err)
				}
			}
			if err := CreateWorktree(ctx, plan, lock); err == nil {
				t.Fatal("destination changed but checkout started")
			}
		})
	}
}

func TestWorktreeLockChild(t *testing.T) {
	if os.Getenv("ZENDE_TEST_WORKTREE_LOCK") != "1" {
		return
	}
	lock := os.NewFile(3, "inherited-lock")
	if _, err := lock.Stat(); err != nil {
		os.Exit(2)
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	lock.Close()
	os.Exit(0)
}

func TestWorktreeLockSurvivesOwnerClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := LockWorktree(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorktreeLockChild$")
	cmd.Env = append(os.Environ(), "ZENDE_TEST_WORKTREE_LOCK=1")
	cmd.ExtraFiles = []*os.File{lock}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatal(line, err)
	}
	lock.Close() // equivalent kernel descriptor release to the core exiting
	if other, err := LockWorktree(path); err == nil {
		other.Close()
		t.Fatal("live child lock was bypassed")
	} else {
		var wire *protocol.Error
		if !errors.As(err, &wire) || wire.Data.Kind != "worktree_busy" {
			t.Fatal(err)
		}
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	again, err := LockWorktree(path)
	if err != nil {
		t.Fatal("terminal child still blocks", err)
	}
	again.Close()
}
