package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zende.local/harness/internal/protocol"
)

type launch struct {
	Spec     Spec     `json:"spec"`
	Shell    string   `json:"shell"`
	Limit    int64    `json:"limit"`
	TMUX     string   `json:"tmux"`
	Terminal Terminal `json:"terminal"`
}

type captureRecord struct {
	Version    int         `json:"version"`
	Token      string      `json:"token"`
	PID        int         `json:"pid"`
	Identity   string      `json:"identity"`
	Output     StreamState `json:"output"`
	Done       bool        `json:"done"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	ExitSignal string      `json:"exit_signal,omitempty"`
	Failure    string      `json:"failure,omitempty"`
}

func terminalSocket(root string) string {
	h := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Join("/tmp", "zende-tmux-"+strconv.Itoa(os.Getuid()), hex.EncodeToString(h[:12])+".sock")
}

// Every command selects a private socket and an empty config. No user tmux
// server, config, hooks or session names are used implicitly.
func tmuxCommand(binary, socket string, args ...string) ([]byte, error) {
	return tmuxInput(binary, socket, nil, args...)
}

func tmuxInput(binary, socket string, input io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"-S", socket, "-f", "/dev/null"}, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = input
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TMUX=") && !strings.HasPrefix(v, "TMUX_PANE=") {
			env = append(env, v)
		}
	}
	cmd.Env = env
	return cmd.Output()
}

func (m *Manager) checkTMUX() error {
	if m.tmux == "" {
		return protocol.E("dependency_missing", "PTY requires tmux on PATH; install tmux (Debian/Ubuntu: apt install tmux)")
	}
	if _, err := tmuxCommand(m.tmux, m.socket, "-V"); err != nil {
		return protocol.E("dependency_missing", "tmux could not run; check its installation and PATH")
	}
	dir := filepath.Dir(m.socket)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode().Perm() != 0700 || !ok || int(stat.Uid) != os.Getuid() {
		return protocol.E("invalid_state", "tmux socket directory must be owned by the current user with mode 0700")
	}
	return nil
}

func quoteShell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type paneState struct {
	ServerPID, PID            int
	Session, Pane, Token, TTY string
	Dead                      bool
	ExitCode                  *int
	ExitSignal                string
}

const paneFormat = "#{pid}|#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@zende-token}|#{pane_tty}"

func parsePane(b []byte) (paneState, error) {
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 9 {
		return paneState{}, errors.New("invalid pane metadata")
	}
	s := paneState{Session: parts[1], Pane: parts[2], Dead: parts[4] == "1", ExitSignal: parts[6], Token: parts[7], TTY: parts[8]}
	var err error
	if s.ServerPID, err = strconv.Atoi(parts[0]); err != nil {
		return s, err
	}
	if s.PID, err = strconv.Atoi(parts[3]); err != nil {
		return s, err
	}
	if parts[5] != "" {
		code, err := strconv.Atoi(parts[5])
		if err != nil {
			return s, err
		}
		s.ExitCode = &code
	}
	return s, nil
}

func inspectPane(binary string, t Terminal) (paneState, error) {
	if t.ServerIdentity == "" || identity(t.ServerPID) != t.ServerIdentity {
		return paneState{}, errors.New("tmux server lifetime changed")
	}
	b, err := tmuxCommand(binary, t.Socket, "display-message", "-p", "-t", t.Pane, paneFormat)
	if err != nil {
		return paneState{}, err
	}
	s, err := parsePane(b)
	if err != nil {
		return s, err
	}
	if s.ServerPID != t.ServerPID || s.Pane != t.Pane || s.Session != t.Session || s.Token != t.Token {
		return s, errors.New("tmux pane identity changed")
	}
	return s, nil
}

func (m *Manager) startPTY(e *entry) (State, error) {
	// The pane helper waits on a durable release file. Even a very short command
	// cannot run until the collector has opened and synced its output file.
	l := launch{Spec: e.state.Spec, Shell: m.shell, Limit: m.maxOutput, TMUX: m.tmux,
		Terminal: Terminal{Socket: m.socket, Session: "z-" + e.state.ID, Token: protocol.NewID("pane"), Cols: e.state.Cols, Rows: e.state.Rows}}
	if err := saveJSON(filepath.Join(e.dir, "launch.json"), l); err != nil {
		return m.failStart(e, err)
	}
	b, err := tmuxCommand(m.tmux, m.socket, "new-session", "-d", "-P", "-F", paneFormat,
		"-s", l.Terminal.Session, "-c", l.Spec.Cwd, "-x", strconv.Itoa(l.Spec.Cols), "-y", strconv.Itoa(l.Spec.Rows),
		m.helper, "--zende-internal-pty-worker", e.dir)
	if err != nil {
		state, _ := m.failStart(e, err)
		return state, protocol.E("process_start_failed", "tmux could not create a terminal; check cwd, helper executable and private socket directory")
	}
	p, err := parsePane(b)
	if err != nil {
		return m.failStart(e, err)
	}
	l.Terminal.Pane, l.Terminal.ServerPID = p.Pane, p.ServerPID
	l.Terminal.ServerIdentity = identity(p.ServerPID)
	e.state.Terminal = &l.Terminal
	e.state.PID, e.state.Identity = p.PID, identity(p.PID)
	failed := func(cause error) (State, error) {
		// The user command is still gated, so closing this exact owned session
		// cannot interrupt a command that has already been accepted.
		if l.Terminal.ServerIdentity != "" && identity(p.ServerPID) == l.Terminal.ServerIdentity {
			_, _ = tmuxCommand(m.tmux, m.socket, "kill-session", "-t", "="+l.Terminal.Session)
		}
		return m.failStart(e, cause)
	}
	for _, args := range [][]string{
		{"set-option", "-p", "-t", p.Pane, "remain-on-exit", "on"},
		{"set-option", "-p", "-t", p.Pane, "@zende-token", l.Terminal.Token},
		{"set-option", "-t", p.Pane, "status", "off"},
		{"resize-window", "-t", p.Pane, "-x", strconv.Itoa(l.Spec.Cols), "-y", strconv.Itoa(l.Spec.Rows)},
	} {
		if _, err := tmuxCommand(m.tmux, m.socket, args...); err != nil {
			state, _ := failed(err)
			return state, protocol.E("process_start_failed", fmt.Sprintf("tmux %s failed while preparing PTY", args[0]))
		}
	}
	if err := saveJSON(filepath.Join(e.dir, "launch.json"), l); err != nil {
		return failed(err)
	}
	collector := "exec " + quoteShell(m.helper) + " --zende-internal-pty-capture " + quoteShell(e.dir)
	// pipe-pane expands strftime and tmux formats before invoking the shell.
	collector = strings.NewReplacer("%", "%%", "#", "##").Replace(collector)
	if _, err := tmuxCommand(m.tmux, m.socket, "pipe-pane", "-O", "-t", p.Pane, collector); err != nil {
		return failed(err)
	}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var c captureRecord
		if readJSON(filepath.Join(e.dir, "capture.json"), &c) == nil && c.Version == 1 && c.Token == l.Terminal.Token && c.Identity != "" && identity(c.PID) == c.Identity {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		return failed(errors.New("PTY capture did not become ready"))
	}
	e.state.Status, e.state.StdinOpen = "running", true
	if err := e.save(); err != nil {
		return failed(err)
	}
	if err := saveJSON(filepath.Join(e.dir, "release.json"), l.Terminal.Token); err != nil {
		// rename may have succeeded before directory fsync failed. Do not claim
		// the command never ran or automatically dispatch it a second time.
		e.state.Status, e.state.Failure = "unknown", "PTY release could not be confirmed; inspect the original process"
		_ = e.save()
		return snapshot(e.state), protocol.E("transport_unknown", e.state.Failure)
	}
	return snapshot(e.state), nil
}

func readJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return protocol.StrictDecode(b, value)
}

// refreshPTY is called with e.mu held. The collector's files are authoritative
// for bytes and completion; a runner restart only rebuilds this projection.
func (m *Manager) refreshPTY(e *entry) {
	if e.state.Status == "failed" || e.state.Status == "exited" {
		return
	}
	t := e.state.Terminal
	if t == nil {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "PTY startup identity is incomplete; command will not be repeated"
		return
	}
	var c captureRecord
	if err := readJSON(filepath.Join(e.dir, "capture.json"), &c); err != nil || c.Version != 1 || c.Token != t.Token {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "PTY capture metadata is unavailable"
		return
	}
	previous := e.state.Output["pty"]
	if c.Output.Stored < previous.Stored || c.Output.Stored < 0 || c.Output.Total < c.Output.Stored || c.Output.Total < previous.Total {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "PTY capture byte counters are invalid; previous output metadata is retained"
		return
	}
	e.state.Output["pty"] = c.Output
	if c.Done {
		e.state.StdinOpen, e.state.ExitCode, e.state.ExitSignal = false, c.ExitCode, c.ExitSignal
		e.state.Status, e.state.Failure = "exited", c.Failure
		if c.ExitCode == nil && c.ExitSignal == "" {
			e.state.Status = "unknown"
		}
		if err := e.save(); err != nil {
			e.state.Status, e.state.Failure = "unknown", "PTY completion could not be saved"
		}
		e.finish()
		return
	}
	if c.Identity == "" || identity(c.PID) != c.Identity {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "PTY collector was lost; saved output is retained"
		return
	}
	if _, err := inspectPane(m.tmux, *t); err != nil {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "original tmux pane cannot be verified"
		return
	}
	var release string
	if readJSON(filepath.Join(e.dir, "release.json"), &release) != nil || release != t.Token {
		e.state.Status, e.state.StdinOpen, e.state.Failure = "unknown", false, "PTY launch was not released; inspect before continuing"
		return
	}
	e.state.Status = "running"
}

// RunInternalPTY is used only by child processes launched by the runner. It is
// dispatched before CLI signal handling so exec keeps ordinary terminal signals.
func RunInternalPTY(args []string) (bool, int) {
	if len(args) == 0 || args[0] != "--zende-internal-pty-worker" && args[0] != "--zende-internal-pty-capture" {
		return false, 0
	}
	if len(args) != 2 || !filepath.IsAbs(args[1]) {
		return true, 2
	}
	var l launch
	if err := readJSON(filepath.Join(args[1], "launch.json"), &l); err != nil {
		return true, 1
	}
	var err error
	if args[0] == "--zende-internal-pty-worker" {
		err = runPane(args[1], l)
	} else {
		err = capturePane(args[1], l)
	}
	if err != nil {
		return true, 1
	}
	return true, 0
}

func runPane(dir string, l launch) error {
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		var token string
		if readJSON(filepath.Join(dir, "release.json"), &token) == nil && token == l.Terminal.Token {
			if err := os.Chdir(l.Spec.Cwd); err != nil {
				return err
			}
			return syscall.Exec(l.Shell, []string{l.Shell, "-c", l.Spec.Command}, os.Environ())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("PTY was not released before startup deadline")
}

type captureWriter struct {
	mu     sync.Mutex
	dir    string
	file   *os.File
	limit  int64
	record captureRecord
}

func (w *captureWriter) save() error { return saveJSON(filepath.Join(w.dir, "capture.json"), w.record) }

func (w *captureWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record.Output.Total += int64(len(b))
	keep := min(int64(len(b)), max(int64(0), w.limit-w.record.Output.Stored))
	n, err := w.file.Write(b[:int(keep)])
	w.record.Output.Stored += int64(n)
	if err == nil {
		err = w.file.Sync()
	}
	if err != nil {
		w.record.Failure = "PTY output storage failed"
		_ = w.save()
		return n, err
	}
	if err = w.save(); err != nil {
		return n, err
	}
	return len(b), nil
}

func capturePane(dir string, l launch) error {
	// tmux supplies a Unix socket as stdin. FileConn creates a pollable duplicate,
	// so a timeout can end a read without leaking a blocked OS read goroutine.
	conn, err := net.FileConn(os.Stdin)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = os.Stdin.Close()
	f, err := os.OpenFile(filepath.Join(dir, "pty"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := &captureWriter{dir: dir, file: f, limit: l.Limit, record: captureRecord{Version: 1, Token: l.Terminal.Token, PID: os.Getpid(), Identity: identity(os.Getpid())}}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := w.save(); err != nil {
		return err
	}
	var exit paneState
	var failure string
	var awaitingExit time.Time
	buf := make([]byte, 32768)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, readErr := conn.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				failure = "PTY output capture failed"
				break
			}
		}
		if readErr == nil {
			continue
		}
		var timeout net.Error
		if !errors.As(readErr, &timeout) || !timeout.Timeout() {
			failure = "PTY capture disconnected before exit was confirmed"
			break
		}
		// tmux 3.3a rejects pipe-pane on a dead pane, so waiting for pipe EOF
		// would hang. pane_dead is published only after the PTY and tmux's pipe
		// buffer have drained. One more read-to-empty drains the kernel socket.
		if exit.Dead && (exit.ExitCode != nil || exit.ExitSignal != "") {
			break
		}
		exit, err = inspectPane(l.TMUX, l.Terminal)
		if err != nil {
			failure = "tmux pane was lost; exit and output completeness are unknown"
			break
		}
		if exit.Dead && exit.ExitCode == nil && exit.ExitSignal == "" {
			// tmux can close the PTY before reaping the pane leader. Some
			// environments leave its SIGCHLD unprocessed. A matching Linux
			// zombie still exposes the original wait status; never infer it
			// from a signal we sent or wake tmux by sending another signal.
			exit = recoverPaneExit(dir, l, exit)
			if awaitingExit.IsZero() {
				awaitingExit = time.Now()
			} else if exit.ExitCode == nil && exit.ExitSignal == "" && time.Since(awaitingExit) >= 5*time.Second {
				failure = "PTY closed but the original process exit could not be verified"
				break
			}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record.Done, w.record.Failure = true, failure
	if exit.Dead {
		w.record.ExitCode, w.record.ExitSignal = exit.ExitCode, exit.ExitSignal
	}
	return w.save()
}

func recoverPaneExit(dir string, l launch, pane paneState) paneState {
	var saved State
	if readJSON(filepath.Join(dir, "state.json"), &saved) != nil || saved.SchemaVersion != 1 || saved.Spec != l.Spec || saved.PID != pane.PID || saved.Terminal == nil {
		return pane
	}
	t := saved.Terminal
	if t.Token != l.Terminal.Token || t.Pane != l.Terminal.Pane || t.Session != l.Terminal.Session || t.Socket != l.Terminal.Socket || t.ServerPID != l.Terminal.ServerPID || t.ServerIdentity != l.Terminal.ServerIdentity {
		return pane
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pane.PID), "stat"))
	if err != nil {
		return pane
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return pane
	}
	code, signal, ok := zombieExit(stat, pane.PID, t.ServerPID, saved.Identity, strings.TrimSpace(string(boot)))
	if ok {
		pane.ExitCode, pane.ExitSignal = code, signal
	}
	return pane
}

// /proc/PID/stat fields 3, 4, 22 and 52 are state, parent, start time and
// wait status. Match boot/start identity as well as PID and parent before use.
func zombieExit(stat []byte, pid, parent int, expectedIdentity, boot string) (*int, string, bool) {
	text := string(stat)
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if start < 1 || end <= start || expectedIdentity == "" || boot == "" {
		return nil, "", false
	}
	actualPID, err := strconv.Atoi(strings.TrimSpace(text[:start]))
	if err != nil || actualPID != pid {
		return nil, "", false
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 50 || fields[0] != "Z" || boot+":"+fields[19] != expectedIdentity {
		return nil, "", false
	}
	actualParent, err := strconv.Atoi(fields[1])
	if err != nil || actualParent != parent {
		return nil, "", false
	}
	raw, err := strconv.ParseUint(fields[49], 10, 16)
	if err != nil {
		return nil, "", false
	}
	status := syscall.WaitStatus(raw)
	if status.Exited() {
		code := status.ExitStatus()
		return &code, "", true
	}
	if status.Signaled() {
		return nil, status.Signal().String(), true
	}
	return nil, "", false
}
