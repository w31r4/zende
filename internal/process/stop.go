package process

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"zende.local/harness/internal/protocol"
)

const maxStopItems = 10000

// StopDispatch reserves a dispatched call even when its start RPC has not yet
// reached the runner. These identities remain reserved after scope release.
type StopDispatch struct {
	ID      string `json:"process_id"`
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id"`
}

type StopScope struct {
	Agents  []string       `json:"agents"`
	Pending []StopDispatch `json:"pending"`
}

type StopTarget struct {
	OperationID string `json:"operation_id"`
}

type StopProcess struct {
	StopDispatch
	PTY       bool          `json:"pty"`
	PID       int           `json:"pid,omitempty"`
	Identity  string        `json:"identity,omitempty"`
	State     string        `json:"state"` // pending, unknown, stopped, not_started
	Failure   string        `json:"failure,omitempty"`
	Witnesses []stopWitness `json:"witnesses,omitempty"`
}

type stopWitness = GroupMember

type StopRecord struct {
	Version     int           `json:"version"`
	OperationID string        `json:"operation_id"`
	Fingerprint string        `json:"fingerprint"`
	Scope       StopScope     `json:"scope"`
	State       string        `json:"state"` // frozen, stopped, released
	Processes   []StopProcess `json:"processes"`
}

// StopController belongs to the core/runner boundary, not the model's tool
// catalogue. Freeze precedes termination; release is a separate core decision.
type StopController interface {
	FreezeStop(context.Context, string, StopScope) (StopRecord, error)
	AdvanceStop(context.Context, string) (StopRecord, error)
	InspectStop(context.Context, string) (StopRecord, error)
	ReleaseStop(context.Context, string) (StopRecord, error)
}

func canonicalStop(scope StopScope) (StopScope, string, error) {
	bad := func() (StopScope, string, error) {
		return StopScope{}, "", protocol.E("invalid_params", "stop scope requires distinct Agent and dispatched process identities (maximum 10000 each)")
	}
	if len(scope.Agents) == 0 || len(scope.Agents) > maxStopItems || len(scope.Pending) > maxStopItems {
		return bad()
	}
	scope.Agents = slices.Clone(scope.Agents)
	scope.Pending = append([]StopDispatch{}, scope.Pending...)
	slices.Sort(scope.Agents)
	slices.SortFunc(scope.Pending, func(a, b StopDispatch) int { return strings.Compare(a.ID, b.ID) })
	for i, id := range scope.Agents {
		if !processID.MatchString(id) || i > 0 && scope.Agents[i-1] == id {
			return bad()
		}
	}
	for i, p := range scope.Pending {
		if !processID.MatchString(p.ID) || !processID.MatchString(p.TaskID) || !slices.Contains(scope.Agents, p.AgentID) || i > 0 && scope.Pending[i-1].ID == p.ID {
			return bad()
		}
	}
	req, err := protocol.NewRequest("process.stop.freeze", "", scope)
	if err != nil {
		return StopScope{}, "", err
	}
	fingerprint, err := req.Fingerprint()
	return scope, fingerprint, err
}

func cloneStop(r StopRecord) StopRecord {
	r.Scope.Agents = slices.Clone(r.Scope.Agents)
	r.Scope.Pending = slices.Clone(r.Scope.Pending)
	r.Processes = slices.Clone(r.Processes)
	for i := range r.Processes {
		r.Processes[i].Witnesses = slices.Clone(r.Processes[i].Witnesses)
	}
	return r
}

func stopGate(op string) error {
	err := protocol.E("tree_stopping", "Agent processes are frozen by an unfinished tree stop")
	err.Data.OperationID = op
	return err
}

func stopFailure(err error) string {
	var wire *protocol.Error
	if errors.As(err, &wire) {
		return wire.Message
	}
	return "kernel process scope could not be read; termination remains unverified"
}

// Both admission checks require m.mu. Existing start/control receipts are
// returned before these checks; neither retry starts another process/write.
func (m *Manager) ownerAllowed(agent string) error {
	if m.stopFault {
		return protocol.E("invalid_state", "runner stop metadata is unreadable; new process effects are held, original files preserved")
	}
	for _, r := range m.stops {
		if r.State != "released" && slices.Contains(r.Scope.Agents, agent) {
			return stopGate(r.OperationID)
		}
	}
	return nil
}

func (m *Manager) startAllowed(p Spec) error {
	if err := m.ownerAllowed(p.AgentID); err != nil {
		return err
	}
	for _, r := range m.stops {
		for _, target := range r.Processes {
			if target.ID != p.ID {
				continue
			}
			err := protocol.E("operation_unknown", "original process metadata is unavailable; its stopped process_id remains reserved")
			if target.State == "not_started" {
				err = protocol.E("process_cancelled", "dispatch was cancelled before process start; process_id remains reserved")
			}
			err.Data.OperationID = r.OperationID
			return err
		}
	}
	return nil
}

// Kernel exit proof releases capacity even when a restarted pipe runner cannot
// recover an exit code. Keep its original unknown/lost output state unchanged.
// Caller holds m.mu; process identities are never reused after release.
func (m *Manager) stoppedProcess(id string) bool {
	if m.stopFault {
		return false
	}
	for _, r := range m.stops {
		for _, p := range r.Processes {
			if p.ID == id && p.State == "stopped" {
				return true
			}
		}
	}
	return false
}

func (m *Manager) stopPath(id string) string {
	return filepath.Join(m.root, ".stops", id+".json")
}

// Called once while constructing the manager. Unknown/corrupt records hold
// admission rather than disappearing from the stop scope. Queries still work.
func (m *Manager) loadStops() {
	m.stops = map[string]StopRecord{}
	dir := filepath.Join(m.root, ".stops")
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || !st.IsDir() {
		m.stopFault = true
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		m.stopFault = true
		return
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".state-") {
			continue // Atomic-write scratch files never authorize effects.
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if !strings.HasSuffix(file.Name(), ".json") || !processID.MatchString(id) {
			m.stopFault = true
			continue
		}
		r, err := readStop(filepath.Join(dir, file.Name()))
		if err != nil || r.OperationID != id {
			m.stopFault = true
			continue
		}
		m.stops[id] = r
	}
}

func readStop(path string) (StopRecord, error) {
	var r StopRecord
	bad := protocol.E("invalid_state", "runner stop record is unreadable or inconsistent; original file preserved")
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > protocol.MaxFrameBytes {
		return r, bad
	}
	f, err := os.Open(path)
	if err != nil {
		return r, bad
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, protocol.MaxFrameBytes+1))
	if err != nil || len(data) > protocol.MaxFrameBytes || protocol.StrictDecode(data, &r) != nil {
		return r, bad
	}
	_, fp, err := canonicalStop(r.Scope)
	if err != nil || r.Version != 1 || !processID.MatchString(r.OperationID) || fp != r.Fingerprint || (r.State != "frozen" && r.State != "stopped" && r.State != "released") || len(r.Processes) > maxStopItems {
		return r, bad
	}
	seen := map[string]StopProcess{}
	for _, p := range r.Processes {
		if !processID.MatchString(p.ID) || !slices.Contains(r.Scope.Agents, p.AgentID) || p.PID < 0 || len(p.Witnesses) > maxStopItems {
			return r, bad
		}
		if _, ok := seen[p.ID]; ok {
			return r, bad
		}
		seen[p.ID] = p
		switch p.State {
		case "not_started":
			if p.PID != 0 || p.Identity != "" || len(p.Witnesses) != 0 {
				return r, bad
			}
		case "stopped":
		case "pending", "unknown":
			if r.State != "frozen" {
				return r, bad
			}
		default:
			return r, bad
		}
		for _, w := range p.Witnesses {
			if w.PID <= 1 || w.Identity == "" {
				return r, bad
			}
		}
	}
	for _, dispatch := range r.Scope.Pending {
		if p, ok := seen[dispatch.ID]; !ok || p.StopDispatch != dispatch {
			return r, bad
		}
	}
	return r, nil
}

// Caller holds stopMu. Save before updating the projection. A failed fsync may
// follow a successful rename: hold all new effects until recovery verifies it.
func (m *Manager) saveStop(r StopRecord) error {
	data, err := json.Marshal(r)
	if err != nil || len(data) > protocol.MaxFrameBytes-4096 {
		return protocol.E("output_limit", "runner stop record exceeds the IPC budget")
	}
	if err := saveJSON(m.stopPath(r.OperationID), r); err != nil {
		m.mu.Lock()
		m.stopFault = true
		m.mu.Unlock()
		return protocol.E("transport_unknown", "runner stop receipt could not be confirmed; inspect the original operation")
	}
	m.mu.Lock()
	m.stops[r.OperationID] = cloneStop(r)
	m.mu.Unlock()
	return nil
}

func (m *Manager) FreezeStop(ctx context.Context, op string, scope StopScope) (StopRecord, error) {
	if !processID.MatchString(op) {
		return StopRecord{}, protocol.E("invalid_params", "stable stop operation_id is required")
	}
	scope, fingerprint, err := canonicalStop(scope)
	if err != nil {
		return StopRecord{}, err
	}
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	m.admission.Lock()
	defer m.admission.Unlock()
	if err := ctx.Err(); err != nil {
		return StopRecord{}, err
	}
	m.mu.Lock()
	if m.stopFault {
		m.mu.Unlock()
		return StopRecord{}, protocol.E("invalid_state", "runner stop metadata could not be verified; admission remains held")
	}
	if old, ok := m.stops[op]; ok {
		m.mu.Unlock()
		if old.Fingerprint != fingerprint {
			return StopRecord{}, protocol.E("operation_conflict", "stop operation_id identifies another scope")
		}
		return cloneStop(old), nil
	}
	r, err := m.captureStop(op, fingerprint, scope)
	m.mu.Unlock()
	if err != nil {
		return StopRecord{}, err
	}
	if err := os.MkdirAll(filepath.Join(m.root, ".stops"), 0700); err != nil {
		return StopRecord{}, err
	}
	if st, err := os.Lstat(filepath.Join(m.root, ".stops")); err != nil || !st.IsDir() {
		return StopRecord{}, protocol.E("invalid_state", "runner stop directory is not a directory; original files preserved")
	}
	// Persist the new subdirectory as well as the record inside it.
	dir, err := os.Open(m.root)
	if err != nil {
		return StopRecord{}, err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return StopRecord{}, err
	}
	if err := m.saveStop(r); err != nil {
		return StopRecord{}, err
	}
	return cloneStop(r), nil
}

// captureStop runs under admission's exclusive lock and m.mu. Every start is
// either already represented here or rejected by the persisted owner fence.
func (m *Manager) captureStop(op, fingerprint string, scope StopScope) (StopRecord, error) {
	r := StopRecord{Version: 1, OperationID: op, Fingerprint: fingerprint, Scope: scope, State: "frozen", Processes: []StopProcess{}}
	for _, agent := range scope.Agents {
		if err := m.ownerAllowed(agent); err != nil {
			return r, err
		}
	}
	seen := map[string]StopProcess{}
	for _, e := range m.entries {
		e.mu.Lock()
		s := snapshot(e.state)
		quarantined := e.quarantined
		e.mu.Unlock()
		if quarantined {
			return r, protocol.E("operation_unknown", "process ownership metadata is unavailable; stop scope cannot be proved")
		}
		if !slices.Contains(scope.Agents, s.AgentID) {
			continue
		}
		p := StopProcess{StopDispatch: StopDispatch{ID: s.ID, AgentID: s.AgentID, TaskID: s.TaskID}, PTY: s.PTY, PID: s.PID, Identity: s.Identity, State: "pending"}
		// Failed pipe startup never launched a process. Failed PTY startup may
		// have left a gated helper, so it still requires kernel verification.
		if s.Status == "failed" && !s.PTY && s.PID == 0 {
			p.State = "stopped"
		}
		seen[p.ID] = p
	}
	for _, dispatch := range scope.Pending {
		if p, ok := seen[dispatch.ID]; ok {
			if p.StopDispatch != dispatch {
				return r, protocol.E("operation_conflict", "dispatched process ownership differs from the stop scope")
			}
			continue
		}
		if _, exists := m.entries[dispatch.ID]; exists {
			return r, protocol.E("operation_conflict", "dispatched process belongs to another Agent")
		}
		if err := m.startAllowed(Spec{ID: dispatch.ID, AgentID: dispatch.AgentID}); err != nil {
			// An earlier completed tree stop may already have reserved this ID.
			var wire *protocol.Error
			if !errors.As(err, &wire) || wire.Data.Kind != "process_cancelled" {
				return r, err
			}
			for _, previous := range m.stops[wire.Data.OperationID].Processes {
				if previous.ID == dispatch.ID && previous.StopDispatch != dispatch {
					return r, protocol.E("operation_conflict", "cancelled process identity belongs to another dispatch")
				}
			}
		}
		seen[dispatch.ID] = StopProcess{StopDispatch: dispatch, State: "not_started"}
	}
	if len(seen) > maxStopItems {
		return r, protocol.E("limit_exceeded", "stop scope exceeds 10000 processes; no runner effects were stopped")
	}
	for _, p := range seen {
		r.Processes = append(r.Processes, p)
	}
	slices.SortFunc(r.Processes, func(a, b StopProcess) int { return strings.Compare(a.ID, b.ID) })
	return r, nil
}

func (m *Manager) InspectStop(ctx context.Context, op string) (StopRecord, error) {
	if err := ctx.Err(); err != nil {
		return StopRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopFault {
		return StopRecord{}, protocol.E("invalid_state", "runner stop metadata could not be verified; admission remains held")
	}
	r, ok := m.stops[op]
	if !ok {
		return StopRecord{}, protocol.E("not_found", "runner stop operation not found")
	}
	return cloneStop(r), nil
}

// AdvanceStop sends KILL to verified kernel lifetimes, not numeric PIDs. This
// desired-state operation is retryable without replaying commands/stdin. A
// bounded pass may remain frozen/unknown. Stopped means kernel work ended, not
// that output capture or Task recovery ended.
func (m *Manager) AdvanceStop(ctx context.Context, op string) (StopRecord, error) {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	r, err := m.InspectStop(ctx, op)
	if err != nil || r.State != "frozen" {
		return r, err
	}
	for i := range r.Processes {
		p := &r.Processes[i]
		if p.State == "stopped" || p.State == "not_started" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return r, err
		}
		witnesses, ended, err := stopEvidence(*p)
		if err != nil {
			p.State, p.Failure = "unknown", stopFailure(err)
		} else if ended {
			p.State, p.Failure = "stopped", ""
		} else {
			p.State, p.Witnesses, p.Failure = "pending", witnesses, ""
			// Save evidence before the leader can exit. On runner restart a
			// still-live descendant can prove the original group/session.
			if err := m.saveStop(r); err != nil {
				return r, err
			}
			if err := killStopMembers(*p); err != nil {
				p.State, p.Failure = "unknown", stopFailure(err)
			}
		}
		if err := m.saveStop(r); err != nil {
			return r, err
		}
	}
	// Do not keep the caller blocked waiting for an uninterruptible process.
	// The next pass can verify it without sending another model command.
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return r, ctx.Err()
	case <-timer.C:
	}
	complete := true
	for i := range r.Processes {
		p := &r.Processes[i]
		if p.State != "stopped" && p.State != "not_started" {
			if _, ended, err := stopEvidence(*p); err == nil && ended {
				p.State, p.Failure = "stopped", ""
			} else if err != nil {
				p.State, p.Failure = "unknown", stopFailure(err)
			}
		}
		complete = complete && (p.State == "stopped" || p.State == "not_started")
	}
	if complete {
		r.State = "stopped"
	}
	err = m.saveStop(r)
	return cloneStop(r), err
}

func (m *Manager) ReleaseStop(ctx context.Context, op string) (StopRecord, error) {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	r, err := m.InspectStop(ctx, op)
	if err != nil || r.State == "released" {
		return r, err
	}
	if r.State != "stopped" {
		return r, protocol.E("tree_stopping", "process termination is unverified; scope cannot be released")
	}
	r.State = "released"
	err = m.saveStop(r)
	return cloneStop(r), err
}

var _ StopController = (*Manager)(nil)
