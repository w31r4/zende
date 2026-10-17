package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"zende.local/harness/internal/protocol"
)

type Manager struct {
	admission             sync.RWMutex // Orders scope freezes with starts and input dispatch.
	stopMu                sync.Mutex
	mu                    sync.Mutex
	root, shell, instance string
	maxProcesses          int
	maxOutput             int64
	entries               map[string]*entry
	tmux, socket          string
	helper                string
	cache                 *outputCache
	stops                 map[string]StopRecord
	stopFault             bool // Damaged stop metadata must not silently reopen admission.
	shutdown              bool
	configRefreshMu       sync.Mutex
	configSource          ConfigurationSource // Set before serving.
	configPublication     *protocol.ConfigPublication
	configError           *protocol.Error
	configReady           bool
	configLoaded          bool
	configFault           bool // Preserve unreadable or future configuration metadata.
}

type entry struct {
	mu          sync.Mutex
	inputMu     sync.Mutex
	state       State
	dir         string
	command     *exec.Cmd
	stdin       *os.File
	done        chan struct{}
	finished    sync.Once
	quarantined bool // Invalid metadata is inspectable but never rewritten.
}

var processID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$`)

func NewManager(root, shell string, maxProcesses int, maxOutput int64) (*Manager, error) {
	settings := Configuration{Shell: shell, MaxProcesses: maxProcesses, MaxOutput: maxOutput}
	if !filepath.IsAbs(root) || !settings.Valid() {
		return nil, fmt.Errorf("invalid runner settings")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	m := &Manager{root: root, shell: shell, instance: protocol.NewID("runner"), maxProcesses: maxProcesses, maxOutput: maxOutput, entries: map[string]*entry{}, configReady: true}
	m.loadConfiguration()
	m.tmux, _ = exec.LookPath("tmux")
	m.helper, _ = os.Executable()
	m.socket = terminalSocket(root)
	m.cache = newOutputCache(4 << 20)
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if !dir.IsDir() || !processID.MatchString(dir.Name()) {
			continue
		}
		e := recoverEntry(filepath.Join(root, dir.Name()), dir.Name())
		m.entries[dir.Name()] = e
	}
	m.loadStops()
	return m, nil
}

func (e *entry) save() error {
	if e.quarantined {
		return protocol.E("invalid_state", "process metadata is quarantined; original files are preserved")
	}
	return saveJSON(filepath.Join(e.dir, "state.json"), e.state)
}

func (e *entry) finish() { e.finished.Do(func() { close(e.done) }) }

func saveJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func snapshot(s State) State {
	out := s
	if s.Configuration != nil {
		configuration := cloneConfiguration(*s.Configuration)
		out.Configuration = &configuration
	}
	out.References = make(map[string]OutputReference, len(s.References))
	for k, v := range s.References {
		out.References[k] = v
	}
	if s.Terminal != nil {
		t := *s.Terminal
		out.Terminal = &t
	}
	out.Output = map[string]StreamState{}
	for k, v := range s.Output {
		out.Output[k] = v
	}
	if s.ExitCode != nil {
		x := *s.ExitCode
		out.ExitCode = &x
	}
	return out
}

// Start is idempotent by process identity. Its context bounds acceptance, not
// the lifetime of the command; disconnecting core must not cancel the process.
func (m *Manager) Start(ctx context.Context, spec Spec) (State, error) {
	m.admission.RLock()
	defer m.admission.RUnlock()
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if !processID.MatchString(spec.ID) || !filepath.IsAbs(spec.Cwd) || spec.Command == "" {
		return State{}, protocol.E("invalid_params", "process_id, absolute cwd and command are required")
	}
	if spec.PTY {
		if spec.Cols == 0 {
			spec.Cols = 80
		}
		if spec.Rows == 0 {
			spec.Rows = 24
		}
		if spec.Cols < 2 || spec.Rows < 2 || spec.Cols > 1000 || spec.Rows > 1000 {
			return State{}, protocol.E("invalid_params", "terminal size must be between 2 and 1000 cells per axis")
		}
	} else if spec.Cols != 0 || spec.Rows != 0 {
		return State{}, protocol.E("invalid_params", "terminal size does not apply to pipe")
	}
	m.mu.Lock()
	state, found, err := m.existingStartLocked(spec)
	m.mu.Unlock()
	if found {
		return state, err
	}
	// An accepted identity is independent of today's configuration/Core. New
	// identities synchronize before admission, then recheck for a racing retry.
	syncErr := m.syncConfiguration(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, found, err := m.existingStartLocked(spec); found {
		return state, err
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if m.shutdown {
		return State{}, protocol.E("unavailable", "runner is stopping; existing process identities remain inspectable")
	}
	if !m.configReady || m.configFault {
		return State{}, protocol.E("unavailable", "runner configuration is not ready; inspect runner status; existing processes are retained")
	}
	if syncErr != nil && (m.configPublication != nil || !runnerCoreOffline(syncErr)) {
		return State{}, syncErr
	}
	if err := m.startAllowed(spec); err != nil {
		return State{}, err
	}
	active := 0
	for _, e := range m.entries {
		e.mu.Lock()
		if e.state.PTY {
			m.refreshPTY(e)
		}
		if (e.state.Status == "running" || e.state.Status == "starting" || e.state.Status == "unknown") && !m.stoppedProcess(e.state.ID) {
			active++
		}
		e.mu.Unlock()
	}
	if active >= m.maxProcesses {
		return State{}, protocol.E("limit_exceeded", "process capacity reached; existing output is retained")
	}
	if spec.PTY {
		if err := m.checkTMUX(); err != nil {
			return State{}, err
		}
	}
	dir := filepath.Join(m.root, spec.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return State{}, err
	}
	configuration := m.configurationLocked()
	e := &entry{state: State{Spec: spec, SchemaVersion: 2, Configuration: &configuration, InstanceID: m.instance, Status: "starting", Output: map[string]StreamState{"stdout": {}, "stderr": {}}}, dir: dir, done: make(chan struct{})}
	e.state.CreatedAt = time.Now().UnixNano()
	e.state.References = map[string]OutputReference{}
	if spec.TaskID != "" {
		e.state.References["task:"+spec.TaskID] = OutputReference{Kind: "task", Owner: spec.TaskID, Held: true}
	}
	if spec.PTY {
		e.state.Output = map[string]StreamState{"pty": {}}
	}
	// Persist intent before starting. A crash in the dispatch window cannot cause
	// the same ID to launch another command on recovery.
	if err := e.save(); err != nil {
		return State{}, err
	}
	m.entries[spec.ID] = e
	if spec.PTY {
		return m.startPTY(e)
	}
	stdout, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return m.failStart(e, err)
	}
	stderr, err := os.OpenFile(filepath.Join(dir, "stderr"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		stdout.Close()
		return m.failStart(e, err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return m.failStart(e, err)
	}
	command := exec.Command(configuration.Shell, "-c", spec.Command)
	command.Dir = spec.Cwd
	command.Env = processEnvironment(spec, os.Environ())
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdin = read
	command.Stdout = &outputWriter{entry: e, file: stdout, stream: "stdout", limit: configuration.MaxOutput}
	command.Stderr = &outputWriter{entry: e, file: stderr, stream: "stderr", limit: configuration.MaxOutput}
	command.WaitDelay = 2 * time.Second
	e.mu.Lock()
	if err := command.Start(); err != nil {
		e.mu.Unlock()
		read.Close()
		write.Close()
		stdout.Close()
		stderr.Close()
		return m.failStart(e, err)
	}
	read.Close()
	e.command = command
	e.stdin = write
	e.state.Status = "running"
	e.state.PID = command.Process.Pid
	e.state.Identity = identity(e.state.PID)
	e.state.StdinOpen = true
	persistErr := e.save()
	state = snapshot(e.state)
	e.mu.Unlock()
	go func() {
		err := command.Wait()
		write.Close()
		stdout.Close()
		stderr.Close()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.state.StdinOpen = false
		e.state.Status = "exited"
		code := command.ProcessState.ExitCode()
		e.state.ExitCode = &code
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			e.state.ExitSignal = status.Signal().String()
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			e.state.Failure = "descendant kept output open beyond capture deadline"
		}
		if err := e.save(); err != nil {
			e.state.Status = "unknown"
			e.state.Failure = "exit result could not be persisted"
		}
		e.finish()
	}()
	if persistErr != nil {
		return state, protocol.E("transport_unknown", "process started but its identity could not be saved; inspect before retrying")
	}
	return state, nil
}

func (m *Manager) existingStartLocked(spec Spec) (State, bool, error) {
	e, ok := m.entries[spec.ID]
	if !ok {
		return State{}, false, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.quarantined {
		return snapshot(e.state), true, protocol.E("operation_unknown", "original process metadata is unavailable; the process_id cannot be reused")
	}
	if e.state.Spec != spec {
		return State{}, true, protocol.E("operation_conflict", "process_id already identifies another command")
	}
	if e.state.PTY {
		m.refreshPTY(e)
	}
	return snapshot(e.state), true, nil
}

func (m *Manager) failStart(e *entry, cause error) (State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.Status = "failed"
	e.state.Failure = "command could not start"
	if err := e.save(); err != nil {
		return snapshot(e.state), err
	}
	e.finish()
	return snapshot(e.state), protocol.E("process_start_failed", "command could not start; inspect cwd and shell")
}

type outputWriter struct {
	entry  *entry
	file   *os.File
	stream string
	limit  int64
}

func (w *outputWriter) Write(data []byte) (int, error) {
	e := w.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	output := e.state.Output[w.stream]
	output.Total += int64(len(data))
	stored := int64(0)
	for _, s := range e.state.Output {
		stored += s.Stored
	}
	keep := min(int64(len(data)), max(int64(0), w.limit-stored))
	n, err := w.file.Write(data[:int(keep)])
	output.Stored += int64(n)
	e.state.Output[w.stream] = output
	if err != nil {
		e.state.Failure = "output storage failed"
		_ = e.save()
		return n, err
	}
	if err := w.file.Sync(); err != nil {
		return n, err
	}
	if err := e.save(); err != nil {
		return n, err
	}
	// Continue draining after the limit. The missing suffix is explicit in Total.
	return len(data), nil
}

func (m *Manager) get(id string) (*entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, protocol.E("not_found", "process not found")
	}
	return e, nil
}
func (m *Manager) Inspect(_ context.Context, id string) (State, error) {
	e, err := m.get(id)
	if err != nil {
		return State{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.PTY {
		m.refreshPTY(e)
	}
	return snapshot(e.state), nil
}

func (m *Manager) Wait(ctx context.Context, id string, duration time.Duration) (State, error) {
	e, err := m.get(id)
	if err != nil {
		return State{}, err
	}
	if duration < 0 || duration > 30*time.Second {
		return State{}, protocol.E("invalid_params", "wait must be between zero and 30 seconds")
	}
	if e.state.PTY {
		deadline := time.NewTimer(duration)
		defer deadline.Stop()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			state, err := m.Inspect(ctx, id)
			if err != nil || state.Status != "running" && state.Status != "starting" {
				return state, err
			}
			select {
			case <-ctx.Done():
				return State{}, ctx.Err()
			case <-deadline.C:
				return state, nil
			case <-tick.C:
			}
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return State{}, ctx.Err()
	case <-e.done:
	case <-timer.C:
	}
	return m.Inspect(ctx, id)
}

func (m *Manager) Read(ctx context.Context, id, stream string, offset int64, limit int) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if offset < 0 || limit < 1 || limit > 1<<20 {
		return Output{}, protocol.E("invalid_params", "invalid output offset or limit")
	}
	e, err := m.get(id)
	if err != nil {
		return Output{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.quarantined {
		return Output{}, protocol.E("output_missing", "output metadata is unavailable; original files are preserved")
	}
	if e.state.PTY {
		m.refreshPTY(e)
	}
	s, ok := e.state.Output[stream]
	if !ok {
		return Output{}, protocol.E("invalid_params", "unknown output stream")
	}
	if offset > s.Total {
		return Output{}, protocol.E("cursor_gap", "output cursor is ahead of the process")
	}
	out := Output{ProcessID: id, Stream: stream, Next: offset, Total: s.Total, Status: e.state.Status, Data: []byte{}}
	if e.state.ReleasedBy != "" {
		if offset < s.Total {
			out.Gap, out.Next = &Gap{From: offset, To: s.Total}, s.Total
		}
		return out, nil
	}
	if offset < s.Stored {
		file, err := os.Open(filepath.Join(e.dir, stream))
		if err != nil {
			return Output{}, protocol.E("output_missing", "saved output file is unavailable")
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() < s.Stored {
			return Output{}, protocol.E("output_missing", "saved output range is incomplete")
		}
		data := make([]byte, min(int64(limit), s.Stored-offset))
		key := outputKey(file.Name(), offset, len(data), stat)
		n := len(data)
		if cached, ok := m.cache.get(key); ok {
			copy(data, cached)
		} else {
			n, err = file.ReadAt(data, offset)
			if err == nil {
				m.cache.put(key, data[:n])
			}
		}
		if err != nil && err != io.EOF {
			return Output{}, err
		}
		out.Data = data[:n]
		out.Text = DisplayText(out.Data)
		out.Next += int64(n)
		if int64(n) < int64(len(data)) {
			return Output{}, protocol.E("output_missing", "saved output range is incomplete")
		}
	}
	if out.Next >= s.Stored && s.Total > out.Next {
		out.Gap = &Gap{From: out.Next, To: s.Total}
		out.Next = s.Total
	}
	return out, nil
}

// identity combines boot ID and kernel process start time, not PID alone.
func identity(pid int) string {
	if pid <= 0 {
		return ""
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 {
		return ""
	}
	fields := strings.Fields(string(stat)[i+1:])
	if len(fields) < 20 {
		return ""
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(boot)) + ":" + fields[19]
}
