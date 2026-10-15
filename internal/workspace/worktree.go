package workspace

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

// The lock descriptor is inherited by Git. If core dies, a new core cannot
// verify/publish the directory while the original checkout is still writing.
// Keep the lock inode on disk; unlinking it would permit two independent locks.
func LockWorktree(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		failure := protocol.E("worktree_busy", "original workspace operation is still running; retry the same request")
		failure.Data.Retryable = true
		return nil, failure
	}
	return f, nil // Close, not LOCK_UN: a surviving child still owns this lock.
}

func worktreeGit(ctx context.Context, cwd string, lock *os.File, args ...string) ([]byte, error) {
	return trackedWorktreeGit(ctx, cwd, lock, nil, args...)
}

func trackedWorktreeGit(ctx context.Context, cwd string, lock *os.File, started func(process.Group) error, args ...string) ([]byte, error) {
	query, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	options := []string{"--no-pager", "--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "gc.auto=0"}
	cmd := exec.CommandContext(query, "git", append(options, args...)...)
	cmd.Dir, cmd.Env = cwd, gitEnvironment(os.Environ())
	cmd.WaitDelay = 100 * time.Millisecond
	if lock != nil {
		cmd.ExtraFiles = []*os.File{lock}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	out, diagnostic := &boundedOutput{limit: maxGitOutput, cancel: cancel}, &boundedOutput{limit: 16 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	if lock != nil {
		// Creation needs only its exit status. Direct descriptors avoid SIGPIPE
		// (and Git's failure cleanup) when core dies during checkout. Diagnostic
		// text may contain configured filter secrets, so it is not persisted.
		discard, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		defer discard.Close()
		cmd.Stdout, cmd.Stderr = discard, discard
	}
	err := cmd.Start()
	var saveErr error
	if err == nil {
		if started != nil {
			group, _ := process.CaptureGroup(cmd.Process.Pid)
			// A fast exit can lose its /proc identity. Still record the PID;
			// the operation lock and absence check decide cancellation later.
			saveErr = started(group)
			if saveErr != nil {
				observed, ended, observeErr := process.ObserveGroup(group)
				if observeErr == nil && !ended {
					_ = process.KillGroup(observed)
				}
				// Kill uses the original os.Process handle if group observation
				// failed. Any surviving filter still retains the operation lock.
				_ = cmd.Process.Kill()
			}
		}
		err = cmd.Wait()
	}
	if saveErr != nil {
		return nil, protocol.E("persistence_failed", "Git process identity could not be saved; inspect the original operation")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if query.Err() != nil || out.exceeded || diagnostic.exceeded {
		return nil, protocol.E("worktree_limit", "Git workspace command exceeded its time or output limit")
	}
	if err != nil {
		if strings.HasPrefix(diagnostic.String(), "fatal: not a git repository") {
			return nil, protocol.E("worktree_not_git", "worktree requires a Git repository; shared cwd was not selected")
		}
		return nil, protocol.E("worktree_git", "Git workspace command failed; existing directories are preserved")
	}
	return out.Bytes(), nil
}

func PrepareWorktree(ctx context.Context, source, target string) (protocol.Worktree, error) {
	var plan protocol.Worktree
	if !filepath.IsAbs(source) || target == "" || !validPath(target) {
		return plan, protocol.E("invalid_params", "worktree requires a source directory and explicit target path")
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return plan, protocol.E("directory_unavailable", "worktree source is unavailable")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(canonical, target)
	}
	target = filepath.Clean(target)
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return plan, protocol.E("directory_unavailable", "worktree target parent must already exist")
	}
	target = filepath.Join(parent, filepath.Base(target))
	if !validPath(target) {
		return plan, protocol.E("invalid_params", "worktree path exceeds 4096 bytes")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return plan, protocol.E("worktree_exists", "worktree target must not exist, even as an empty directory or symlink")
	}
	common, err := worktreeGit(ctx, canonical, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return plan, err
	}
	repository, err := filepath.EvalSymlinks(strings.TrimSuffix(string(common), "\n"))
	if err != nil {
		return plan, protocol.E("worktree_git", "Git repository directory is unavailable")
	}
	if rel, err := filepath.Rel(repository, target); err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return plan, protocol.E("invalid_params", "worktree target must be outside Git's administrative directory")
	}
	head, err := worktreeGit(ctx, canonical, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return plan, protocol.E("worktree_no_head", "worktree requires an existing HEAD commit")
	}
	commit := strings.TrimSpace(string(head))
	oid, err := hex.DecodeString(commit)
	if err != nil || len(oid) != 20 && len(oid) != 32 {
		return plan, protocol.E("worktree_git", "Git returned an invalid commit identity")
	}
	return protocol.Worktree{Source: canonical, Repository: repository, Commit: commit, Path: target, Owner: protocol.NewID("zende-worktree")}, nil
}

func CreateWorktree(ctx context.Context, plan protocol.Worktree, lock *os.File) error {
	return CreateTrackedWorktree(ctx, plan, lock, nil)
}

// started runs after Git starts and before Wait, while core and Git both hold
// the operation lock. A lost core never authorizes a second checkout.
func CreateTrackedWorktree(ctx context.Context, plan protocol.Worktree, lock *os.File, started func(process.Group) error) error {
	if lock == nil {
		return protocol.E("worktree_busy", "workspace creation requires its operation lock")
	}
	// A failed preflight or externally-created directory is never overwritten.
	parent, err := filepath.EvalSymlinks(filepath.Dir(plan.Path))
	if err != nil || parent != filepath.Dir(plan.Path) {
		return protocol.E("worktree_changed", "worktree target parent changed")
	}
	if _, err := os.Lstat(plan.Path); !errors.Is(err, os.ErrNotExist) {
		return protocol.E("worktree_exists", "worktree target appeared after preparation; preserved without changes")
	}
	_, err = trackedWorktreeGit(ctx, plan.Repository, lock, started, "--git-dir="+plan.Repository, "worktree", "add", "--detach", "--lock", "--reason="+plan.Owner, "--", plan.Path, plan.Commit)
	return err
}

// A saved successful creation permits later edits. Recovery without that
// receipt requires the expected commit and a clean index/checkout as evidence.
// This function never creates, resets, unlocks, prunes or removes a worktree.
func VerifyWorktree(ctx context.Context, plan protocol.Worktree, recovering bool) error {
	unknown := func() error {
		return protocol.E("worktree_unknown", "worktree identity or completed checkout could not be verified; inspect the original directory and operation, do not recreate it")
	}
	info, err := os.Lstat(plan.Path)
	if err != nil || !info.IsDir() {
		return unknown()
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(plan.Path))
	if err != nil || parent != filepath.Dir(plan.Path) {
		return unknown()
	}
	common, err := worktreeGit(ctx, plan.Path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return unknown()
	}
	canonical, err := filepath.EvalSymlinks(strings.TrimSuffix(string(common), "\n"))
	if err != nil || canonical != plan.Repository {
		return unknown()
	}
	// The checkout's .git file must select its own linked-worktree metadata,
	// not another worktree in the same repository with a different index.
	gitDir, err := worktreeGit(ctx, plan.Path, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return unknown()
	}
	admin, err := filepath.EvalSymlinks(strings.TrimSuffix(string(gitDir), "\n"))
	if err != nil || filepath.Dir(admin) != filepath.Join(plan.Repository, "worktrees") {
		return unknown()
	}
	if !worktreeMetadataEquals(filepath.Join(admin, "gitdir"), filepath.Join(plan.Path, ".git")+"\n") ||
		!worktreeMetadataEquals(filepath.Join(admin, "locked"), plan.Owner+"\n") {
		return unknown()
	}
	data, err := worktreeGit(ctx, plan.Repository, nil, "--git-dir="+plan.Repository, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return unknown()
	}
	matched := false
	for _, record := range bytes.Split(data, []byte{0, 0}) {
		fields := map[string]string{}
		for _, field := range bytes.Split(record, []byte{0}) {
			key, value, _ := strings.Cut(string(field), " ")
			fields[key] = value
		}
		if fields["worktree"] == plan.Path && fields["locked"] == plan.Owner && (!recovering || fields["HEAD"] == plan.Commit) {
			matched = true
		}
	}
	if !matched {
		return unknown()
	}
	if recovering {
		status, err := Git(ctx, plan.Path, "")
		if err != nil || status.State != "ready" || status.Commit != plan.Commit || !status.Detached || status.Staged+status.Unstaged+status.Untracked+status.Conflicts != 0 {
			return unknown()
		}
	}
	return nil
}

func worktreeMetadataEquals(path, want string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(want)) {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(len(want))+1))
	return err == nil && string(data) == want
}
