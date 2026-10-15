package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"zende.local/harness/internal/protocol"
)

type OutputReference struct {
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
	Held  bool   `json:"held"`
}
type ReferenceParams struct {
	ID          string `json:"process_id"`
	ReferenceID string `json:"reference_id"`
	Kind        string `json:"kind"`
	Owner       string `json:"owner"`
	Hold        bool   `json:"hold"`
}
type RetentionStatus struct {
	StoredBytes     int64 `json:"stored_bytes"`
	ProtectedBytes  int64 `json:"protected_bytes"`
	EligibleBytes   int64 `json:"eligible_bytes"`
	HeldReferences  int   `json:"held_references"`
	MaxProcessBytes int64 `json:"max_process_bytes"`
	CacheBytes      int   `json:"cache_bytes"`
	CacheLimit      int   `json:"cache_limit"`
}
type RotateParams struct {
	RetainBytes int64 `json:"retain_bytes"`
}
type RotationTarget struct {
	ID        string `json:"process_id"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
}
type Rotation struct {
	Version        int              `json:"version"`
	OperationID    string           `json:"operation_id"`
	RetainBytes    int64            `json:"retain_bytes"`
	Status         string           `json:"status"`
	CreatedAt      int64            `json:"created_at"`
	Targets        []RotationTarget `json:"targets"`
	FreedBytes     int64            `json:"freed_bytes"`
	RemainingBytes int64            `json:"remaining_bytes"`
}

type RotationTargetParams struct {
	OperationID string `json:"operation_id"`
}

func streamNames(s State) []string {
	if s.PTY {
		return []string{"pty"}
	}
	return []string{"stdout", "stderr"}
}
func validStreams(s State) bool {
	expected := streamNames(s)
	if len(s.Output) != len(expected) {
		return false
	}
	for _, name := range expected {
		v, ok := s.Output[name]
		if !ok || v.Stored < 0 || v.Total < v.Stored {
			return false
		}
	}
	return true
}
func heldCount(s State) int {
	n := 0
	for _, ref := range s.References {
		if ref.Held {
			n++
		}
	}
	return n
}
func protected(s State) bool {
	return s.Status != "exited" && s.Status != "failed" || heldCount(s) != 0
}

// Only these fixed filenames are output data. Metadata never supplies an
// arbitrary filesystem path to a deletion operation.
func storedBytes(e *entry) (int64, error) {
	var total int64
	names := streamNames(e.state)
	if e.quarantined {
		names = []string{"stdout", "stderr", "pty"}
	}
	for _, name := range names {
		st, err := os.Lstat(filepath.Join(e.dir, name))
		if os.IsNotExist(err) && (e.quarantined || e.state.ReleasedBy != "" || e.state.Output[name].Stored == 0) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return 0, protocol.E("output_missing", "output storage is missing or is not a regular file")
		}
		total += st.Size()
	}
	return total, nil
}

func (m *Manager) Reference(ctx context.Context, op string, p ReferenceParams) (ControlResult, error) {
	if p.ReferenceID == "" || len(p.ReferenceID) > 256 || p.Owner == "" || len(p.Owner) > 256 || p.Kind != "task" && p.Kind != "history" && p.Kind != "manual" {
		return ControlResult{}, protocol.E("invalid_params", "reference_id, owner and kind (task, history or manual) are required")
	}
	return m.control(ctx, p.ID, op, "process.output.reference", p, func(e *entry) (ControlResult, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		result := ControlResult{State: snapshot(e.state)}
		if e.state.ReleasedBy != "" {
			return result, protocol.E("output_released", "output was already released; it cannot be pinned again")
		}
		if e.state.References == nil {
			e.state.References = map[string]OutputReference{}
		}
		if old, ok := e.state.References[p.ReferenceID]; ok && (old.Kind != p.Kind || old.Owner != p.Owner) {
			return result, protocol.E("operation_conflict", "reference identity belongs to another owner")
		}
		e.state.References[p.ReferenceID] = OutputReference{Kind: p.Kind, Owner: p.Owner, Held: p.Hold}
		if err := e.save(); err != nil {
			return result, protocol.E("transport_unknown", "output reference could not be saved; inspect original operation")
		}
		result.State = snapshot(e.state)
		return result, nil
	})
}

// retentionLocked holds m.mu; each process projection has its own lock.
func (m *Manager) retentionLocked() (RetentionStatus, []RotationTarget, error) {
	status := RetentionStatus{MaxProcessBytes: m.maxOutput, CacheLimit: m.cache.limit}
	m.cache.mu.Lock()
	status.CacheBytes = m.cache.used
	m.cache.mu.Unlock()
	var eligible []RotationTarget
	for id, e := range m.entries {
		e.mu.Lock()
		if e.state.PTY {
			m.refreshPTY(e)
		}
		size, err := storedBytes(e)
		if err != nil {
			e.mu.Unlock()
			return status, nil, err
		}
		status.StoredBytes += size
		status.HeldReferences += heldCount(e.state)
		if protected(e.state) {
			status.ProtectedBytes += size
		} else {
			status.EligibleBytes += size
			if size > 0 && e.state.ReleasedBy == "" {
				eligible = append(eligible, RotationTarget{ID: id, Bytes: size, CreatedAt: e.state.CreatedAt})
			}
		}
		e.mu.Unlock()
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].CreatedAt == eligible[j].CreatedAt {
			return eligible[i].ID < eligible[j].ID
		}
		return eligible[i].CreatedAt < eligible[j].CreatedAt
	})
	return status, eligible, nil
}
func (m *Manager) Retention(ctx context.Context) (RetentionStatus, error) {
	if err := ctx.Err(); err != nil {
		return RetentionStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status, _, err := m.retentionLocked()
	return status, err
}

func (m *Manager) RotationInspect(ctx context.Context, op string) (Rotation, error) {
	if err := ctx.Err(); err != nil {
		return Rotation{}, err
	}
	if !processID.MatchString(op) {
		return Rotation{}, protocol.E("invalid_params", "valid operation_id is required")
	}
	var record Rotation
	if err := readJSON(filepath.Join(m.root, ".retention", op+".json"), &record); err != nil {
		return record, protocol.E("not_found", "rotation operation not found or unreadable")
	}
	if record.Version != 1 || record.OperationID != op {
		return record, protocol.E("invalid_state", "rotation record is invalid")
	}
	return record, nil
}

func (m *Manager) rotationPlanLocked(p RotateParams) (Rotation, error) {
	if p.RetainBytes < 0 {
		return Rotation{}, protocol.E("invalid_params", "retain_bytes must be nonnegative")
	}
	status, candidates, err := m.retentionLocked()
	if err != nil {
		return Rotation{}, err
	}
	if status.ProtectedBytes > p.RetainBytes {
		return Rotation{}, protocol.E("limit_exceeded", "retention target is below protected output; release eligible references or choose a larger target")
	}
	record := Rotation{Version: 1, RetainBytes: p.RetainBytes, Status: "planned", CreatedAt: time.Now().UnixNano(), RemainingBytes: status.StoredBytes}
	for _, target := range candidates {
		if record.RemainingBytes <= p.RetainBytes {
			break
		}
		record.Targets = append(record.Targets, target)
		record.RemainingBytes -= target.Bytes
	}
	if record.RemainingBytes > p.RetainBytes {
		return record, protocol.E("limit_exceeded", "unfinished output release prevents the requested retention target")
	}
	return record, nil
}

// PlanRotation is advisory. Actual rotation rechecks references under lock.
func (m *Manager) PlanRotation(ctx context.Context, p RotateParams) (Rotation, error) {
	if err := ctx.Err(); err != nil {
		return Rotation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rotationPlanLocked(p)
}

// Rotate is explicit, oldest-first and whole-process. It does not clear model
// history or reference records. Planned targets survive a restart; a retry only
// finishes that original plan and never selects newly unreferenced output.
func (m *Manager) Rotate(ctx context.Context, op string, p RotateParams) (Rotation, error) {
	if !processID.MatchString(op) || p.RetainBytes < 0 {
		return Rotation{}, protocol.E("invalid_params", "stable operation_id and nonnegative retain_bytes are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Rotation{}, err
	}
	dir := filepath.Join(m.root, ".retention")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Rotation{}, err
	}
	path := filepath.Join(dir, op+".json")
	var record Rotation
	if err := readJSON(path, &record); err == nil {
		if record.Version != 1 || record.OperationID != op || record.Status != "pending" && record.Status != "completed" {
			return record, protocol.E("invalid_state", "rotation record is invalid")
		}
		if record.RetainBytes != p.RetainBytes {
			return record, protocol.E("operation_conflict", "operation_id identifies another retention target")
		}
		if record.Status == "completed" {
			return record, nil
		}
	} else if !os.IsNotExist(err) {
		return record, protocol.E("invalid_state", "rotation record is unreadable")
	} else {
		plan, err := m.rotationPlanLocked(p)
		if err != nil {
			return record, err
		}
		record = plan
		record.OperationID, record.Status = op, "pending"
		if err := saveJSON(path, record); err != nil {
			return record, err
		}
	}
	for _, target := range record.Targets {
		if err := ctx.Err(); err != nil {
			return record, err
		}
		e, ok := m.entries[target.ID]
		if !ok {
			return record, protocol.E("invalid_state", "rotation target no longer has process metadata")
		}
		e.mu.Lock()
		err := m.releaseOutputLocked(e, op)
		e.mu.Unlock()
		if err != nil {
			return record, err
		}
	}
	status, _, err := m.retentionLocked()
	if err != nil {
		return record, err
	}
	record.RemainingBytes = status.StoredBytes
	record.FreedBytes = 0
	for _, target := range record.Targets {
		record.FreedBytes += target.Bytes
	}
	record.Status = "completed"
	if err := saveJSON(path, record); err != nil {
		return record, protocol.E("transport_unknown", "rotation result could not be saved; retry the same operation_id")
	}
	return record, nil
}

func (m *Manager) releaseOutputLocked(e *entry, op string) error {
	if protected(e.state) {
		return protocol.E("references_in_use", "output acquired a reference after planning; no referenced file was removed")
	}
	if e.state.ReleasedBy != "" && e.state.ReleasedBy != op {
		return protocol.E("operation_conflict", "another rotation owns this output release")
	}
	if e.state.ReleasedBy == "" {
		e.state.ReleasedBy = op
		if err := e.save(); err != nil {
			return protocol.E("transport_unknown", "output release intent could not be saved")
		}
	}
	// Tombstone first, then unlink only the validated fixed stream names. If the
	// runner exits between them, reads report the release and the same operation
	// can finish removing its own files. The command cannot run a second time.
	for _, stream := range streamNames(e.state) {
		path := filepath.Join(e.dir, stream)
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return protocol.E("output_missing", "release target is not a regular output file")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return protocol.E("release_failed", "output file could not be removed; retry the original rotation")
		}
	}
	d, err := os.Open(e.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	m.cache.forgetPrefix(e.dir + string(filepath.Separator))
	return nil
}
