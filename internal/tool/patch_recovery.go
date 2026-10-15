package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/patch"
	"zende.local/harness/internal/protocol"
)

// The receipt is independent of the core transaction: a crash after editing but
// before tool.completed can recover the exact result, without reading new file
// contents as evidence of an old execution. Pending receipts never authorize a
// replay, even when the visible files happen to match the patch's desired state.
type patchRecord struct {
	Version     int             `json:"version"`
	OperationID string          `json:"operation_id"`
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"`
	Result      Result          `json:"result"`
	Error       *protocol.Error `json:"error,omitempty"`
}

var patchID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

const maxPatchRecord = model.MaxBlockBytes + 4096

func patchIdentity(call Call, text string) string {
	data, _ := json.Marshal([]string{call.AgentID, call.TaskID, call.Cwd, text})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (t patchTool) recordPath(call Call) (string, error) {
	if !filepath.IsAbs(t.journal) || !patchID.MatchString(call.ID) {
		return "", protocol.E("invalid_params", "patch requires an absolute journal directory and a stable operation_id")
	}
	return filepath.Join(t.journal, call.ID+".json"), nil
}

func unknownPatch() error {
	return protocol.E("operation_unknown", "original patch has no verified result; inspect its operation and files, do not reapply it")
}

func checkedPatchRecord(path string, call Call, text string) (patchRecord, error) {
	var r patchRecord
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return r, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPatchRecord {
		return r, unknownPatch()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPatchRecord+1))
	if err != nil || len(data) > maxPatchRecord || protocol.StrictDecode(data, &r) != nil || r.Version != 1 || r.OperationID != call.ID || r.Status != "pending" && r.Status != "completed" {
		return r, unknownPatch()
	}
	if r.Fingerprint != patchIdentity(call, text) {
		return r, protocol.E("operation_conflict", "patch operation belongs to different arguments, directory or task")
	}
	if r.Status == "completed" {
		var report patch.Report
		if r.Result.ProcessID != "" || len(r.Result.Content) != 1 || r.Result.Content[0].Type != "text" || protocol.StrictDecode([]byte(r.Result.Content[0].Text), &report) != nil {
			return r, unknownPatch()
		}
		switch report.Status {
		case "applied":
			if r.Error != nil {
				return r, unknownPatch()
			}
		case "rejected", "failed", "partial":
			if r.Error == nil || r.Error.Data.Kind == "" {
				return r, unknownPatch()
			}
		default:
			return r, unknownPatch()
		}
	}
	return r, nil
}

func (t patchTool) executeRecorded(ctx context.Context, call Call, text string) (Result, error) {
	path, err := t.recordPath(call)
	if err != nil {
		return Result{}, err
	}
	unlock, err := lockPatchJournal(ctx, t.journal)
	if err != nil {
		return Result{}, protocol.E("patch_io", "patch journal is unavailable; no edit was dispatched")
	}
	defer unlock()
	if r, err := checkedPatchRecord(path, call, text); err == nil {
		if r.Status != "completed" || uncertainPatch(r.Error) {
			return r.Result, unknownPatch()
		}
		if r.Error != nil {
			return r.Result, r.Error
		}
		return r.Result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// The file may describe an already applied effect. Never replace it.
		return Result{}, unknownPatch()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, protocol.E("patch_cancelled", "patch cancelled before dispatch")
	}
	r := patchRecord{Version: 1, OperationID: call.ID, Fingerprint: patchIdentity(call, text), Status: "pending"}
	if err := t.save(path, r); err != nil {
		return Result{}, protocol.E("patch_io", "patch intent could not be saved; no edit was dispatched")
	}
	result, actionErr := t.applyPatch(ctx, call.Cwd, text)
	r.Result, r.Status = result, "completed"
	if actionErr != nil && !errors.As(actionErr, &r.Error) {
		return result, unknownPatch()
	}
	if err := t.save(path, r); err != nil {
		return result, unknownPatch()
	}
	return result, actionErr
}

func uncertainPatch(err *protocol.Error) bool {
	return err != nil && (err.Data.Kind == "operation_unknown" || err.Data.Kind == "transport_unknown")
}

// Recover is strictly read-only. A saved partial failure is a known result;
// an interrupted engine, failed durability check or missing receipt is not.
func (t patchTool) Recover(ctx context.Context, call Call) (Recovered, error) {
	if err := ctx.Err(); err != nil {
		return Recovered{}, err
	}
	text, err := patchText(call)
	if err != nil {
		return Recovered{}, err
	}
	path, err := t.recordPath(call)
	if err != nil {
		return Recovered{}, unknownPatch()
	}
	r, err := checkedPatchRecord(path, call, text)
	if err != nil || r.Status != "completed" || uncertainPatch(r.Error) {
		return Recovered{}, unknownPatch()
	}
	return Recovered{Result: r.Result, Error: r.Error}, nil
}

func syncPatchDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func savePatchRecord(path string, r patchRecord) error {
	data, err := json.Marshal(r)
	if err != nil || len(data) > maxPatchRecord {
		return errors.New("patch receipt exceeds its limit")
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		err = syncPatchDirectory(dir)
	}
	return err
}

// The elected core is the usual single writer. Flock also makes duplicate
// calls safe across test helpers or overlapping processes during replacement.
// The profile state parent must already exist; this never creates workspace dirs.
func lockPatchJournal(ctx context.Context, dir string) (func(), error) {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := syncPatchDirectory(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

var _ Recoverer = patchTool{}
