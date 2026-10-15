package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

const MaxInputBytes = 64 << 10

type InputParams struct {
	ID    string `json:"process_id"`
	Data  []byte `json:"data"`
	Close bool   `json:"close"`
}
type ResizeParams struct {
	ID   string `json:"process_id"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}
type SignalParams struct {
	ID     string `json:"process_id"`
	Signal string `json:"signal"`
}
type ControlTarget struct {
	ID          string `json:"process_id"`
	OperationID string `json:"operation_id"`
}
type ControlResult struct {
	OperationID string `json:"operation_id"`
	State       State  `json:"state"`
	Written     int    `json:"written"`
}
type ControlRecord struct {
	Version     int             `json:"version"`
	OperationID string          `json:"operation_id"`
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"`
	Result      ControlResult   `json:"result"`
	Error       *protocol.Error `json:"error,omitempty"`
}

func (m *Manager) ControlInspect(_ context.Context, p ControlTarget) (ControlRecord, error) {
	e, err := m.get(p.ID)
	if err != nil {
		return ControlRecord{}, err
	}
	if !processID.MatchString(p.OperationID) {
		return ControlRecord{}, protocol.E("invalid_params", "valid operation_id is required")
	}
	var record ControlRecord
	if err := readJSON(filepath.Join(e.dir, "controls", p.OperationID+".json"), &record); err != nil {
		return record, protocol.E("not_found", "process control operation not found or unreadable")
	}
	return record, nil
}

// Serialize a process's mutations and persist dispatch intent before touching
// the external process. A pending operation after a crash is never replayed.
func (m *Manager) control(ctx context.Context, id, op, method string, params any, act func(*entry) (ControlResult, error)) (ControlResult, error) {
	// Freeze waits for already admitted writes. A late stdin request must not
	// create more work after a scope has been verified stopped.
	if method == "process.stdin" || method == "process.resize" {
		m.admission.RLock()
		defer m.admission.RUnlock()
	}
	if !processID.MatchString(op) {
		return ControlResult{}, protocol.E("invalid_params", "stable operation_id is required")
	}
	e, err := m.get(id)
	if err != nil {
		return ControlResult{}, err
	}
	if e.quarantined {
		return ControlResult{}, protocol.E("invalid_state", "process metadata is unavailable; original files are preserved")
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ControlResult{}, err
	}
	req, err := protocol.NewRequest(method, op, params)
	if err != nil {
		return ControlResult{}, err
	}
	fingerprint, err := req.Fingerprint()
	if err != nil {
		return ControlResult{}, err
	}
	dir := filepath.Join(e.dir, "controls")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ControlResult{}, err
	}
	path := filepath.Join(dir, op+".json")
	var record ControlRecord
	if err := readJSON(path, &record); err == nil {
		if record.Version != 1 || record.OperationID != op || record.Status != "pending" && record.Status != "completed" {
			return ControlResult{}, protocol.E("invalid_state", "unsupported process control record")
		}
		if record.Fingerprint != fingerprint {
			return ControlResult{}, protocol.E("operation_conflict", "operation_id identifies different process control parameters")
		}
		if record.Status == "pending" {
			return record.Result, protocol.E("operation_unknown", "process control dispatch is uncertain; inspect before sending more input")
		}
		if record.Error != nil {
			return record.Result, record.Error
		}
		return record.Result, nil
	} else if !os.IsNotExist(err) {
		return ControlResult{}, protocol.E("invalid_state", "process control record is unreadable")
	}
	// A new ID must not bypass an unresolved input/resize. Signalling remains
	// available so a user can always stop their own uncertain process.
	if method == "process.stdin" || method == "process.resize" {
		m.mu.Lock()
		admitErr := m.ownerAllowed(e.state.AgentID)
		m.mu.Unlock()
		if admitErr != nil {
			return ControlResult{}, admitErr
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return ControlResult{}, err
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			var previous ControlRecord
			if err := readJSON(filepath.Join(dir, f.Name()), &previous); err != nil {
				return ControlResult{}, protocol.E("invalid_state", "process control history is unreadable")
			}
			if previous.Status == "pending" {
				return ControlResult{}, protocol.E("operation_unknown", "an earlier process control is uncertain; inspect it before continuing")
			}
		}
	}
	record = ControlRecord{Version: 1, OperationID: op, Fingerprint: fingerprint, Status: "pending"}
	if err := saveJSON(path, record); err != nil {
		return ControlResult{}, err
	}
	result, actionErr := act(e)
	result.OperationID = op
	record.Result, record.Status = result, "completed"
	if actionErr != nil {
		if !errors.As(actionErr, &record.Error) {
			record.Error = protocol.E("process_control_failed", "process control failed; query its operation record")
		}
		// An external dispatch timeout may have applied its effects. Preserve
		// the pending barrier rather than allowing a new ID to blindly retry.
		if record.Error.Data.Kind == "transport_unknown" {
			record.Status = "pending"
		}
	}
	if err := saveJSON(path, record); err != nil {
		return result, protocol.E("transport_unknown", "process control result could not be saved; inspect original operation")
	}
	if record.Error != nil {
		return result, record.Error
	}
	return result, nil
}

func (m *Manager) Input(ctx context.Context, op string, p InputParams) (ControlResult, error) {
	if len(p.Data) > MaxInputBytes || len(p.Data) == 0 && !p.Close {
		return ControlResult{}, protocol.E("invalid_params", "stdin requires data or close; maximum input is 64 KiB")
	}
	return m.control(ctx, p.ID, op, "process.stdin", p, func(e *entry) (ControlResult, error) {
		state, err := m.Inspect(ctx, p.ID)
		if err != nil {
			return ControlResult{}, err
		}
		result := ControlResult{State: state}
		if state.Status != "running" || !state.StdinOpen {
			return result, protocol.E("input_closed", "original process no longer accepts input")
		}
		if state.PTY {
			pane, err := inspectPane(m.tmux, *state.Terminal)
			if err != nil || pane.Dead {
				return result, protocol.E("process_unavailable", "original terminal is not running")
			}
			data := append([]byte(nil), p.Data...)
			if p.Close {
				fd, err := unix.Open(pane.TTY, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
				if err != nil {
					return result, protocol.E("process_unavailable", "terminal mode could not be read")
				}
				mode, err := unix.IoctlGetTermios(fd, unix.TCGETS)
				_ = unix.Close(fd)
				if err != nil || mode.Lflag&unix.ICANON == 0 || mode.Cc[unix.VEOF] == 0 {
					return result, protocol.E("unsupported", "PTY close requires canonical mode with EOF enabled; use explicit input or terminate in raw mode")
				}
				// A terminal has no pipe-style half close. Two VEOF characters
				// flush any partial line, then indicate EOF on the next read.
				data = append(data, mode.Cc[unix.VEOF], mode.Cc[unix.VEOF])
			}
			buffer := protocol.NewID("input")
			if _, err := tmuxInput(m.tmux, state.Terminal.Socket, bytes.NewReader(data), "load-buffer", "-b", buffer, "-"); err != nil {
				return result, protocol.E("process_control_failed", "tmux input buffer could not be prepared")
			}
			defer tmuxCommand(m.tmux, state.Terminal.Socket, "delete-buffer", "-b", buffer)
			if _, err := tmuxCommand(m.tmux, state.Terminal.Socket, "paste-buffer", "-r", "-d", "-b", buffer, "-t", state.Terminal.Pane); err != nil {
				return result, protocol.E("transport_unknown", "tmux input dispatch is uncertain; do not resend blindly")
			}
			result.Written = len(p.Data)
		} else {
			if e.stdin == nil {
				return result, protocol.E("input_closed", "pipe input descriptor was lost")
			}
			deadline := time.Now().Add(2 * time.Second)
			if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
				deadline = d
			}
			if err := e.stdin.SetWriteDeadline(deadline); err != nil {
				return result, err
			}
			n, writeErr := e.stdin.Write(p.Data)
			result.Written = n
			if writeErr != nil {
				err = protocol.E("input_partial", fmt.Sprintf("stdin accepted %d bytes; inspect the operation before sending the remaining bytes", n))
			}
			if p.Close && writeErr == nil {
				err = e.stdin.Close()
			}
		}
		e.mu.Lock()
		e.state.InputBytes += int64(result.Written)
		if p.Close && err == nil {
			e.state.StdinOpen = false
		}
		persistErr := e.save()
		result.State = snapshot(e.state)
		e.mu.Unlock()
		if persistErr != nil {
			return result, protocol.E("transport_unknown", "stdin state could not be saved; inspect original operation")
		}
		return result, err
	})
}

func (m *Manager) Resize(ctx context.Context, op string, p ResizeParams) (ControlResult, error) {
	if p.Cols < 2 || p.Rows < 2 || p.Cols > 1000 || p.Rows > 1000 {
		return ControlResult{}, protocol.E("invalid_params", "terminal size must be between 2 and 1000 cells per axis")
	}
	return m.control(ctx, p.ID, op, "process.resize", p, func(e *entry) (ControlResult, error) {
		state, err := m.Inspect(ctx, p.ID)
		if err != nil {
			return ControlResult{}, err
		}
		result := ControlResult{State: state}
		if !state.PTY {
			return result, protocol.E("unsupported", "resize does not apply to pipe processes")
		}
		if state.Status != "running" {
			return result, protocol.E("process_unavailable", "original terminal is not running")
		}
		pane, err := inspectPane(m.tmux, *state.Terminal)
		if err != nil || pane.Dead {
			return result, protocol.E("process_unavailable", "original terminal cannot be verified")
		}
		if _, err := tmuxCommand(m.tmux, state.Terminal.Socket, "resize-window", "-t", state.Terminal.Pane, "-x", strconv.Itoa(p.Cols), "-y", strconv.Itoa(p.Rows)); err != nil {
			return result, protocol.E("transport_unknown", "tmux resize result is uncertain; inspect original operation")
		}
		fd, err := unix.Open(pane.TTY, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err != nil {
			return result, protocol.E("transport_unknown", "resized terminal could not be queried")
		}
		defer unix.Close(fd)
		confirmed := false
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err == nil && int(size.Col) == p.Cols && int(size.Row) == p.Rows {
				confirmed = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !confirmed {
			return result, protocol.E("transport_unknown", "kernel terminal size did not confirm the resize")
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.state.Terminal.Cols, e.state.Terminal.Rows = p.Cols, p.Rows
		result.State = snapshot(e.state)
		if err := e.save(); err != nil {
			return result, protocol.E("transport_unknown", "terminal size could not be saved")
		}
		return result, nil
	})
}

func (m *Manager) Signal(ctx context.Context, op string, p SignalParams) (ControlResult, error) {
	signal, ok := map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL}[p.Signal]
	if !ok {
		return ControlResult{}, protocol.E("invalid_params", "signal must be INT, TERM or KILL")
	}
	return m.control(ctx, p.ID, op, "process.signal", p, func(e *entry) (ControlResult, error) {
		state, err := m.Inspect(ctx, p.ID)
		if err != nil {
			return ControlResult{}, err
		}
		result := ControlResult{State: state}
		if state.Status == "exited" || state.Status == "failed" {
			return result, nil
		}
		if state.Identity == "" || identity(state.PID) != state.Identity {
			return result, protocol.E("process_unavailable", "original process identity cannot be verified; no signal sent")
		}
		pgid, err := syscall.Getpgid(state.PID)
		if err != nil || pgid != state.PID || pgid == syscall.Getpgrp() {
			return result, protocol.E("process_unavailable", "owned process group cannot be verified; no signal sent")
		}
		groups := map[int]int{pgid: state.PID}
		if state.PTY {
			if _, err := inspectPane(m.tmux, *state.Terminal); err != nil {
				return result, protocol.E("process_unavailable", "original terminal cannot be verified; no signal sent")
			}
			// Interactive shells may put foreground/background jobs in other
			// groups, all inside this pane's unique POSIX session.
			if sid, err := unix.Getsid(state.PID); err != nil || sid != state.PID {
				return result, protocol.E("process_unavailable", "terminal session identity changed")
			}
			entries, err := os.ReadDir("/proc")
			if err != nil {
				return result, err
			}
			for _, proc := range entries {
				pid, err := strconv.Atoi(proc.Name())
				if err != nil {
					continue
				}
				if sid, err := unix.Getsid(pid); err == nil && sid == state.PID {
					if group, err := syscall.Getpgid(pid); err == nil && group > 1 && group != syscall.Getpgrp() {
						groups[group] = pid
					}
				}
			}
		}
		// Signal jobs before the pane leader, which may reap them on exit.
		for group, member := range groups {
			if group == pgid {
				continue
			}
			if sid, err := unix.Getsid(member); err != nil || sid != state.PID {
				continue
			}
			if err := syscall.Kill(-group, signal); err != nil && err != syscall.ESRCH {
				return result, protocol.E("transport_unknown", "some process groups could not be signalled; inspect before repeating")
			}
		}
		if identity(state.PID) == state.Identity {
			if err := syscall.Kill(-pgid, signal); err != nil && err != syscall.ESRCH {
				return result, protocol.E("process_control_failed", "process group could not be signalled")
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.state.CancelSignal = p.Signal
		result.State = snapshot(e.state)
		if err := e.save(); err != nil {
			return result, protocol.E("transport_unknown", "signal request result could not be saved")
		}
		return result, nil
	})
}
