package workspace

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const maxGitOutput = 2 << 20

// Git uses no shell, network, diff driver, fsmonitor hook or optional index
// refresh. Environment repository overrides must not redirect this query.
func Git(ctx context.Context, base, path string) (protocol.GitStatus, error) {
	return gitStatus(ctx, base, path, "git", 3*time.Second)
}

func gitStatus(ctx context.Context, base, path, executable string, timeout time.Duration) (protocol.GitStatus, error) {
	if err := ctx.Err(); err != nil {
		return protocol.GitStatus{}, err
	}
	dir, err := DirectoryPath(base, path)
	if err != nil {
		return protocol.GitStatus{}, err
	}
	query, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(query, executable,
		"--no-pager", "--no-optional-locks",
		"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "core.hooksPath=/dev/null", "-c", "status.renames=false",
		"status", "--porcelain=v2", "-z", "--branch", "--ahead-behind",
		"--untracked-files=normal", "--ignore-submodules=all")
	cmd.Dir, cmd.Env = dir, gitEnvironment(os.Environ())
	cmd.WaitDelay = 100 * time.Millisecond
	out := &boundedOutput{limit: maxGitOutput, cancel: cancel}
	diagnostic := &boundedOutput{limit: 16 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	err = cmd.Run()
	if ctx.Err() != nil {
		return protocol.GitStatus{}, ctx.Err()
	}
	if out.exceeded || diagnostic.exceeded {
		return protocol.GitStatus{}, protocol.E("git_limit", "Git status output exceeds the query limit")
	}
	if query.Err() != nil {
		return protocol.GitStatus{}, protocol.E("git_timeout", "Git status exceeded its time limit")
	}
	if err != nil {
		if strings.HasPrefix(diagnostic.String(), "fatal: not a git repository") {
			return protocol.GitStatus{Path: dir, State: "none"}, nil
		}
		// Git stderr can include config values, filesystem paths or control bytes.
		return protocol.GitStatus{}, protocol.E("git_unavailable", "Git status could not be read")
	}
	status, err := parseGitStatus(out.Bytes())
	if err != nil {
		return protocol.GitStatus{}, err
	}
	status.Path = dir
	return status, nil
}

func gitEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+4)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_") || key == "LC_ALL" || key == "LANG" || key == "LANGUAGE" {
			continue
		}
		out = append(out, item)
	}
	return append(out, "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=")
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *boundedOutput) Bytes() []byte  { return w.buffer.Bytes() }
func (w *boundedOutput) String() string { return w.buffer.String() }

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("Git output limit")
	}
	return w.buffer.Write(p)
}

// https://git-scm.com/docs/git-status#_porcelain_format_version_2
// NUL parsing keeps filenames with newlines/ANSI bytes from becoming records.
func parseGitStatus(data []byte) (protocol.GitStatus, error) {
	bad := func() (protocol.GitStatus, error) {
		return protocol.GitStatus{}, protocol.E("git_invalid", "Git returned incomplete or invalid porcelain status")
	}
	if len(data) == 0 || len(data) > maxGitOutput || data[len(data)-1] != 0 {
		return bad()
	}
	result := protocol.GitStatus{State: "ready"}
	seen := map[string]bool{}
	records := bytes.Split(data[:len(data)-1], []byte{0})
	for i := 0; i < len(records); i++ {
		record := string(records[i])
		if strings.HasPrefix(record, "# ") {
			key, value, ok := strings.Cut(record[2:], " ")
			if !ok {
				return bad()
			}
			switch key {
			case "branch.oid", "branch.head", "branch.upstream", "branch.ab":
				if seen[key] || value == "" || !utf8.ValidString(value) {
					return bad()
				}
				seen[key] = true
			}
			switch key {
			case "branch.oid":
				if value == "(initial)" {
					result.Unborn = true
				} else {
					oid, err := hex.DecodeString(value)
					if err != nil || len(oid) != 20 && len(oid) != 32 {
						return bad()
					}
					result.Commit = value
				}
			case "branch.head":
				if value == "(detached)" {
					result.Detached = true
				} else {
					result.Branch = value
				}
			case "branch.upstream":
				result.Upstream = value
			case "branch.ab":
				a, b, ok := strings.Cut(value, " ")
				if !ok || !strings.HasPrefix(a, "+") || !strings.HasPrefix(b, "-") {
					return bad()
				}
				ahead, e1 := strconv.Atoi(a[1:])
				behind, e2 := strconv.Atoi(b[1:])
				if e1 != nil || e2 != nil || ahead < 0 || behind < 0 {
					return bad()
				}
				result.Ahead, result.Behind = &ahead, &behind
			}
			continue
		}
		if len(record) < 3 || record[1] != ' ' {
			return bad()
		}
		switch record[0] {
		case '?':
			result.Untracked++
		case '!':
			// Ignored entries never make a worktree dirty.
		case '1', '2', 'u':
			n := 9
			if record[0] == '2' {
				n = 10
			} else if record[0] == 'u' {
				n = 11
			}
			fields := strings.SplitN(record, " ", n)
			if len(fields) != n || fields[n-1] == "" || len(fields[1]) != 2 {
				return bad()
			}
			xy := fields[1]
			if !strings.ContainsRune(".MADRCUT", rune(xy[0])) || !strings.ContainsRune(".MADRCUT", rune(xy[1])) {
				return bad()
			}
			if record[0] == 'u' {
				result.Conflicts++
			} else {
				if xy[0] != '.' {
					result.Staged++
				}
				if xy[1] != '.' {
					result.Unstaged++
				}
			}
			if record[0] == '2' {
				i++ // original pathname is a separate NUL-delimited field
				if i >= len(records) || len(records[i]) == 0 {
					return bad()
				}
			}
		default:
			return bad()
		}
	}
	if !seen["branch.oid"] || !seen["branch.head"] || result.Detached && result.Unborn || seen["branch.ab"] && !seen["branch.upstream"] {
		return bad()
	}
	return result, nil
}
