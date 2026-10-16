package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

type Info struct {
	Identity
	InstanceID      string          `json:"instance_id"`
	ManifestVersion string          `json:"manifest_version"`
	ProtocolVersion int             `json:"protocol_version,omitempty"`
	Capabilities    []string        `json:"capabilities"`
	State           string          `json:"state"`
	PID             int             `json:"pid,omitempty"`
	Trusted         bool            `json:"trusted_execution"`
	Limits          Limits          `json:"limits"`
	Error           *protocol.Error `json:"error,omitempty"`
}

// Instance owns a process until Closed. The initialization context only bounds
// preparation; a successfully initialized process outlives that context.
// Capability adapters and their call lifetimes are layered on this transport.
type Instance struct {
	cmd    *exec.Cmd
	source string
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	diag   *diagnosticBuffer

	mu            sync.Mutex
	info          Info
	pending       map[string]*pendingResponse
	gate          chan struct{}
	catalog       *Catalog
	subscriptions map[string]*EventSubscription
	once          sync.Once
	closing       sync.Once
	done          chan struct{}
	closed        chan struct{}
	readers       sync.WaitGroup
}

// Start accepts resolved environment values only at the process boundary.
// They are never copied into Info or the initialization request.
// A non-nil instance returned with an error has already been stopped and may
// only be inspected for diagnostics; it must not be published as ready.
func Start(ctx context.Context, source string, manifest Manifest, resolved map[string]string) (*Instance, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return nil, protocol.E("invalid_resource", "plugin manifest source must be an absolute clean path")
	}
	if !slices.Contains(manifest.ProtocolVersions, ProtocolVersion) {
		return nil, protocol.E("version_mismatch", "plugin manifest has no supported protocol version")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifest = manifest.clone()
	if len(resolved) != len(manifest.Env) {
		return nil, protocol.E("unavailable", "plugin environment references are not resolved")
	}
	cmd := exec.Command(manifest.Command, manifest.Args...)
	cmd.Dir = manifest.Cwd
	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(source)
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	var secrets []string
	for key := range manifest.Env {
		value, ok := resolved[key]
		if !ok || value == "" || len(value) > MaxSecretBytes || strings.ContainsRune(value, 0) {
			return nil, protocol.E("unavailable", "plugin environment reference is unavailable or invalid")
		}
		env[key] = value
		secrets = append(secrets, value)
	}
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
		if sensitiveEnv(key) && value != "" {
			if len(value) > MaxSecretBytes {
				return nil, protocol.E("resource_limit", "plugin inherited credential exceeds diagnostic redaction budget")
			}
			secrets = append(secrets, value)
		}
	}
	if len(secrets) > 256 {
		return nil, protocol.E("resource_limit", "too many plugin credential values for diagnostic redaction")
	}
	slices.Sort(cmd.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinReader, stdin, err := os.Pipe()
	if err != nil {
		return nil, protocol.E("unavailable", "plugin input pipe unavailable")
	}
	defer stdinReader.Close()
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, protocol.E("unavailable", "plugin output pipe unavailable")
	}
	defer stdoutWriter.Close()
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, protocol.E("unavailable", "plugin diagnostic pipe unavailable")
	}
	defer stderrWriter.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinReader, stdoutWriter, stderrWriter
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, protocol.E("unavailable", "plugin executable could not start; check command, cwd and dependencies")
	}
	// Parent copies must not keep EOF hidden when the child exits.
	stdinReader.Close()
	stdoutWriter.Close()
	stderrWriter.Close()
	i := &Instance{cmd: cmd, source: source, stdin: stdin, stdout: stdout, stderr: stderr, diag: newDiagnostics(secrets),
		info: Info{Identity: Identity{ID: manifest.ID, Version: manifest.Version}, InstanceID: protocol.NewID("plugin"), ManifestVersion: manifest.Digest(),
			State: "preparing", PID: cmd.Process.Pid, Trusted: true, Limits: DefaultLimits(), Capabilities: []string{}},
		gate: make(chan struct{}, 1), pending: make(map[string]*pendingResponse), done: make(chan struct{}), closed: make(chan struct{})}
	i.readers.Add(2)
	go i.readProtocol()
	go i.readDiagnostics()
	go i.monitor()
	prepare, cancel := context.WithTimeout(ctx, InitializeTimeout)
	defer cancel()
	raw, err := i.exchange(prepare, "plugin.initialize", InitializeParams{
		ProtocolVersions: []int{ProtocolVersion}, Host: Identity{ID: "zende", Version: "1"}, Plugin: i.info.Identity,
		InstanceID: i.info.InstanceID, Capabilities: manifest.Capabilities, Limits: DefaultLimits(),
	})
	var result InitializeResult
	if err == nil && protocol.StrictDecode(raw, &result) != nil {
		err = protocol.E("plugin_protocol", "invalid plugin initialization result")
	}
	if err == nil && result.ProtocolVersion != ProtocolVersion {
		err = protocol.E("version_mismatch", "plugin selected an unsupported protocol version")
	}
	if err == nil {
		caps := slices.Clone(result.Capabilities)
		slices.Sort(caps)
		expected := slices.Clone(manifest.Capabilities)
		slices.Sort(expected)
		if result.Plugin != i.info.Identity || !validCapabilities(result.Capabilities) || !slices.Equal(caps, expected) {
			err = protocol.E("plugin_protocol", "plugin identity or capabilities do not match its manifest")
		}
	}
	i.mu.Lock()
	if err == nil {
		err = prepare.Err()
	}
	if err == nil && i.info.State != "preparing" {
		err = i.info.Error
		if err == nil {
			err = protocol.E("unavailable", "plugin exited before initialization completed")
		}
	}
	if err == nil {
		i.info.State, i.info.ProtocolVersion, i.info.Capabilities = "ready", result.ProtocolVersion, slices.Clone(result.Capabilities)
	}
	i.mu.Unlock()
	if err != nil {
		i.stop(asFailure(err))
		<-i.closed
		return i, err
	}
	return i, nil
}

func asFailure(err error) *protocol.Error {
	var wire *protocol.Error
	if errors.As(err, &wire) {
		return wire
	}
	if errors.Is(err, context.Canceled) {
		return protocol.E("cancelled", "plugin preparation cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.E("timeout", "plugin preparation timed out")
	}
	return protocol.E("plugin_protocol", "plugin transport failed")
}

func (i *Instance) Info() Info {
	i.mu.Lock()
	defer i.mu.Unlock()
	info := i.info
	info.Capabilities = slices.Clone(info.Capabilities)
	if info.Error != nil {
		e := *info.Error
		info.Error = &e
	}
	return info
}

func (i *Instance) Diagnostics() Diagnostics { return i.diag.snapshot() }
func (i *Instance) Done() <-chan struct{}    { return i.done }
func (i *Instance) Closed() <-chan struct{}  { return i.closed }

func (i *Instance) exchange(ctx context.Context, method string, params any) (json.RawMessage, error) {
	pending, _, err := i.startRequest(ctx, method, params, "")
	if err != nil {
		return nil, err
	}
	defer i.forget(pending)
	var reply response
	select {
	case reply = <-pending.ch:
	case <-ctx.Done():
		// Lifecycle/discovery calls have no cancellation handshake. A late
		// reply must not be mistaken for a later request on this connection.
		i.stop(asFailure(ctx.Err()))
		return nil, ctx.Err()
	case <-i.done:
		select {
		case reply = <-pending.ch:
		default:
			return nil, protocol.E("plugin_protocol", "plugin connection ended before its response")
		}
	}
	if len(reply.Error) != 0 {
		if method == "plugin.resource.read" {
			return nil, protocol.E("invalid_resource", "plugin rejected the resource read; inspect diagnostics")
		}
		return nil, protocol.E("plugin_initialization", "plugin rejected the lifecycle request; inspect diagnostics")
	}
	return reply.Result, nil
}

func (i *Instance) readDiagnostics() {
	defer i.readers.Done()
	defer i.diag.write(nil, true)
	buffer := make([]byte, 4096)
	for {
		n, err := i.stderr.Read(buffer)
		if n > 0 {
			i.diag.write(buffer[:n], false)
		}
		if err != nil {
			return
		}
	}
}

func (i *Instance) exited() bool {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, i.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	return err == nil && info.Signo != 0
}

func (i *Instance) monitor() {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-i.done:
			return
		case <-ticker.C:
			if i.exited() {
				i.stop(protocol.E("plugin_exited", "plugin process exited"))
				return
			}
		}
	}
}

func (i *Instance) stop(failure *protocol.Error) {
	i.once.Do(func() {
		i.mu.Lock()
		if failure != nil && i.info.State != "closing" {
			i.info.State, i.info.Error = "failed", failure
		} else {
			i.info.State = "closed"
		}
		i.mu.Unlock()
		close(i.done)
		go func() {
			defer close(i.closed)
			i.stdin.Close()
			_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGTERM)
			deadline := time.Now().Add(300 * time.Millisecond)
			for !i.exited() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			// Keep the leader unreaped until all group signals have been sent.
			_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
			_ = i.cmd.Wait()
			// Drain buffered diagnostics before closing the descriptors. A child
			// that escaped the group can retain a pipe, so draining is bounded.
			drained := make(chan struct{})
			go func() { i.readers.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(100 * time.Millisecond):
			}
			i.stdout.Close()
			i.stderr.Close()
			<-drained
		}()
	})
}

// Close requests a bounded graceful shutdown, then reclaims the owned group.
// It closes transport resources, not any external side effects.
func (i *Instance) Close() error {
	i.closing.Do(func() {
		i.mu.Lock()
		ready := i.info.State == "ready"
		if ready {
			i.info.State = "closing"
		}
		i.mu.Unlock()
		if ready {
			ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
			_, _ = i.exchange(ctx, "plugin.shutdown", struct{}{})
			cancel()
		}
		i.stop(nil)
	})
	<-i.closed
	return nil
}
