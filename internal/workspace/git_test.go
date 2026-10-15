package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Zende Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	// Fixture writes are confined to the temporary repository. User/system Git
	// configuration must not sign commits, invoke hooks or redirect the repo.
	cmd.Env = append(gitEnvironment(os.Environ()), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeFixture(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestGitRealRepositoryCountsAndReadOnlyQueries(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-b", "main")
	ctx := context.Background()
	query := func() protocol.GitStatus {
		t.Helper()
		status, err := Git(ctx, dir, "")
		if err != nil || status.State != "ready" || status.Path != dir {
			t.Fatal(status, err)
		}
		return status
	}
	initial := query()
	if !initial.Unborn || initial.Commit != "" || initial.Branch != "main" || initial.Ahead != nil || initial.Detached {
		t.Fatal(initial)
	}
	file := filepath.Join(dir, "tracked")
	writeFixture(t, file, "original\n", 0600)
	testGit(t, dir, "add", "--", "tracked")
	testGit(t, dir, "commit", "-m", "fixture")
	clean := query()
	if clean.Unborn || clean.Commit == "" || clean.Staged+clean.Unstaged+clean.Conflicts+clean.Untracked != 0 {
		t.Fatal(clean)
	}
	testGit(t, dir, "branch", "upstream")
	testGit(t, dir, "branch", "--set-upstream-to=upstream", "main")
	writeFixture(t, file, "staged\n", 0600)
	testGit(t, dir, "add", "--", "tracked")
	testGit(t, dir, "commit", "-m", "ahead")
	writeFixture(t, file, "new staged\n", 0600)
	testGit(t, dir, "add", "--", "tracked")
	writeFixture(t, file, "and unstaged\n", 0600)
	writeFixture(t, filepath.Join(dir, "odd\n# branch.head FAKE\x1b[31m"), "untracked\n", 0600)
	if err := os.Mkdir(filepath.Join(dir, "untracked-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "untracked-dir", "one"), "1", 0600)
	writeFixture(t, filepath.Join(dir, "untracked-dir", "two"), "2", 0600)
	// A configured fsmonitor executable must never run for a status-bar query.
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	marker := filepath.Join(t.TempDir(), "ran")
	writeFixture(t, hook, "#!/bin/sh\n: > '"+marker+"'\n", 0700)
	testGit(t, dir, "config", "core.fsmonitor", hook)
	index := filepath.Join(dir, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	status := query()
	if status.Branch != "main" || status.Staged != 1 || status.Unstaged != 1 || status.Untracked != 2 || status.Conflicts != 0 || status.Upstream != "upstream" || status.Ahead == nil || *status.Ahead != 1 || status.Behind == nil || *status.Behind != 0 {
		t.Fatal(status)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(index)
	if err != nil || !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("status wrote index", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("status ran fsmonitor", err)
	}
	// A non-Git result contains no prior branch/count fields.
	outside := t.TempDir()
	none, err := Git(ctx, outside, "")
	if err != nil || none.State != "none" || none.Path != outside || none.Branch != "" || none.Commit != "" || none.Staged+none.Unstaged+none.Untracked != 0 {
		t.Fatal(none, err)
	}
	// Environment overrides from the launch shell cannot query another repo.
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	t.Setenv("GIT_WORK_TREE", dir)
	t.Setenv("GIT_INDEX_FILE", index)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", hook)
	none, err = Git(ctx, outside, "")
	if err != nil || none.State != "none" {
		t.Fatal("query followed environment instead of cwd", none, err)
	}
}

func TestGitDetachedLinkedWorktreeAndConflict(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-b", "main")
	file := filepath.Join(dir, "file")
	writeFixture(t, file, "base\n", 0600)
	testGit(t, dir, "add", "--", "file")
	testGit(t, dir, "commit", "-m", "base")
	linked := filepath.Join(t.TempDir(), "linked")
	testGit(t, dir, "worktree", "add", "--detach", linked, "HEAD")
	status, err := Git(context.Background(), linked, "")
	if err != nil || !status.Detached || status.Branch != "" || status.Commit == "" {
		t.Fatal(status, err)
	}
	// Create a real unmerged index without invoking merge hooks or an editor.
	base := testGit(t, dir, "rev-parse", "HEAD:file")
	input := "0 " + strings.Repeat("0", 40) + "\tfile\n100644 " + base + " 1\tfile\n100644 " + base + " 2\tfile\n100644 " + base + " 3\tfile\n"
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir, cmd.Env, cmd.Stdin = dir, gitEnvironment(os.Environ()), strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	status, err = Git(context.Background(), dir, "")
	if err != nil || status.Conflicts != 1 || status.Staged != 0 || status.Unstaged != 0 {
		t.Fatal(status, err)
	}
}

func TestGitQueryBoundsAndRedactedFailure(t *testing.T) {
	dir := t.TempDir()
	makeExe := func(name, body string) string {
		p := filepath.Join(dir, name)
		writeFixture(t, p, "#!/bin/sh\n"+body, 0700)
		return p
	}
	check := func(exe, kind string, timeout time.Duration) {
		t.Helper()
		start := time.Now()
		_, err := gitStatus(context.Background(), dir, "", exe, timeout)
		var wire *protocol.Error
		if !errors.As(err, &wire) || wire.Data.Kind != kind || strings.Contains(err.Error(), "SECRET") || time.Since(start) > 2*time.Second {
			t.Fatal(err, time.Since(start))
		}
	}
	check(filepath.Join(dir, "absent"), "git_unavailable", time.Second)
	check(makeExe("fail", "printf 'SECRET_CONFIG_VALUE\\n' >&2\nexit 1\n"), "git_unavailable", time.Second)
	check(makeExe("hang", "exec sleep 10\n"), "git_timeout", 30*time.Millisecond)
	check(makeExe("too-big", "exec head -c 3000000 /dev/zero\n"), "git_limit", time.Second)
	check(makeExe("invalid", "printf 'invalid\\000'\n"), "git_invalid", time.Second)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Git(cancelled, dir, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	env := gitEnvironment([]string{"GIT_CONFIG_PARAMETERS=secret", "GIT_SSH_COMMAND=secret", "LANG=zh_CN", "LC_ALL=zh_CN", "LANGUAGE=zh", "PATH=/bin", "MY_VAR=keep"})
	if strings.Contains(strings.Join(env, "\n"), "secret") || !strings.Contains(strings.Join(env, "\n"), "MY_VAR=keep") || !strings.Contains(strings.Join(env, "\n"), "GIT_NO_LAZY_FETCH=1") || !strings.Contains(strings.Join(env, "\n"), "GIT_ALLOW_PROTOCOL=") {
		t.Fatal(env)
	}
}

func TestGitPorcelainNULParsing(t *testing.T) {
	head := "# branch.oid " + strings.Repeat("a", 40) + "\x00# branch.head main\x00"
	ordinary := "1 M. N... 100644 100644 100644 " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " file\x00"
	rename := "2 R. N... 100644 100644 100644 " + strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " R100 new\x00? old\n# branch.head fake\x00"
	status, err := parseGitStatus([]byte(head + "# future.extension value\x00" + ordinary + rename + "? newline\nfile\xff\x00"))
	if err != nil || status.Staged != 2 || status.Untracked != 1 || status.Branch != "main" {
		t.Fatal(status, err)
	}
	for _, input := range []string{
		"", head[:len(head)-1], "# branch.head main\x00", head + "# branch.head duplicate\x00",
		head + "# branch.ab +1 -2\x00", head + "# branch.upstream up\x00# branch.ab +oops -0\x00",
		head + "1 ZZ bad\x00", head + ordinary[:len(ordinary)-1], head + strings.TrimSuffix(rename, "? old\n# branch.head fake\x00"),
		head + "unexpected\x00",
	} {
		if _, err := parseGitStatus([]byte(input)); err == nil {
			t.Fatal("invalid status accepted", input)
		}
	}
}

func FuzzGitPorcelain(f *testing.F) {
	f.Add([]byte("# branch.oid (initial)\x00# branch.head main\x00? file\x00"))
	f.Add([]byte("# branch.oid invalid\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		status, err := parseGitStatus(data)
		if err == nil && (status.State != "ready" || status.Commit == "" && !status.Unborn || status.Branch == "" && !status.Detached) {
			t.Fatal("invalid successful projection", status)
		}
	})
}
