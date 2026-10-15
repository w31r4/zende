package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/patch"
	"zende.local/harness/internal/protocol"
)

func recordedPatch(t *testing.T) (patchTool, Call) {
	t.Helper()
	root := t.TempDir()
	args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: marker\n+once\n*** End Patch"})
	return DurablePatchExecutor(filepath.Join(root, "receipts")).(patchTool), Call{
		ID: "patch-original", AgentID: "agent", TaskID: "task", Cwd: root, Arguments: args,
	}
}

func patchErrorKind(err error) string {
	var e *protocol.Error
	if errors.As(err, &e) {
		return e.Data.Kind
	}
	return ""
}

func TestPatchReceiptRecoversAndDeduplicatesWithoutEditing(t *testing.T) {
	p, call := recordedPatch(t)
	var applied atomic.Int32
	engine := p.apply
	p.apply = func(ctx context.Context, cwd, text string) (patch.Report, error) {
		applied.Add(1)
		return engine(ctx, cwd, text)
	}
	var wg sync.WaitGroup
	results := make(chan Result, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := p.Execute(context.Background(), call)
			if err != nil {
				t.Error(err)
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	if applied.Load() != 1 {
		t.Fatal("reapplied", applied.Load())
	}
	first := <-results
	for result := range results {
		if !reflect.DeepEqual(first, result) {
			t.Fatal("different receipts", result)
		}
	}
	// A later external edit does not invalidate historical evidence or invite
	// a replay. Recovery must return the old result and leave current data alone.
	marker := filepath.Join(call.Cwd, "marker")
	if err := os.WriteFile(marker, []byte("later edit"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened := DurablePatchExecutor(p.journal).(patchTool)
	reopened.apply = func(context.Context, string, string) (patch.Report, error) { panic("replayed") }
	recovered, err := reopened.Recover(context.Background(), call)
	if err != nil || recovered.Error != nil || !reflect.DeepEqual(recovered.Result, first) {
		t.Fatal(recovered, err)
	}
	if _, err := reopened.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "later edit" {
		t.Fatal(string(data))
	}
	path, _ := p.recordPath(call)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "*** Begin Patch") || strings.Contains(string(data), "+once") {
		t.Fatal("receipt duplicated patch contents")
	}
}

func TestPatchReceiptFailureBoundariesNeverReplay(t *testing.T) {
	for _, stage := range []string{"intent-save", "after-edit", "result-save"} {
		t.Run(stage, func(t *testing.T) {
			p, call := recordedPatch(t)
			engine, save := p.apply, p.save
			p.apply = func(ctx context.Context, cwd, text string) (patch.Report, error) {
				r, err := engine(ctx, cwd, text)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "after-edit" {
					panic("injected crash after filesystem effect")
				}
				return r, err
			}
			p.save = func(path string, r patchRecord) error {
				if stage == "intent-save" || stage == "result-save" && r.Status == "completed" {
					return errors.New("injected write failure")
				}
				return save(path, r)
			}
			var failure error
			crashed := false
			func() {
				defer func() { crashed = recover() != nil }()
				_, failure = p.Execute(context.Background(), call)
			}()
			if stage == "after-edit" && !crashed || stage == "result-save" && patchErrorKind(failure) != "operation_unknown" || stage == "intent-save" && patchErrorKind(failure) != "patch_io" {
				t.Fatal(crashed, failure)
			}
			data, readErr := os.ReadFile(filepath.Join(call.Cwd, "marker"))
			if stage == "intent-save" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("edited before durable intent", string(data), readErr)
				}
			} else if readErr != nil || string(data) != "once\n" {
				t.Fatal(string(data), readErr)
			}
			reopened := DurablePatchExecutor(p.journal).(patchTool)
			reopened.apply = func(context.Context, string, string) (patch.Report, error) { panic("replayed") }
			if _, err := reopened.Recover(context.Background(), call); patchErrorKind(err) != "operation_unknown" {
				t.Fatal(err)
			}
			if stage != "intent-save" {
				if _, err := reopened.Execute(context.Background(), call); patchErrorKind(err) != "operation_unknown" {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPatchReceiptKeepsKnownFailuresAndUncertainDurability(t *testing.T) {
	for _, code := range []string{"patch_conflict", "patch_io", "patch_cancelled", "operation_unknown"} {
		t.Run(code, func(t *testing.T) {
			p, call := recordedPatch(t)
			p.apply = func(context.Context, string, string) (patch.Report, error) {
				return patch.Report{Status: "partial", Files: []patch.FileResult{{Path: "fixture", Status: "partial", DestinationWritten: true}}}, &patch.Error{Code: code, Message: "injected partial failure"}
			}
			result, err := p.Execute(context.Background(), call)
			if patchErrorKind(err) != code {
				t.Fatal(err)
			}
			r, err := DurablePatchExecutor(p.journal).(Recoverer).Recover(context.Background(), call)
			if code == "operation_unknown" {
				if patchErrorKind(err) != "operation_unknown" {
					t.Fatal(r, err)
				}
			} else if err != nil || patchErrorKind(r.Error) != code || !reflect.DeepEqual(r.Result, result) {
				t.Fatal(r, err)
			}
		})
	}
}

func TestPatchReceiptRejectsChangedIdentityAndCorruption(t *testing.T) {
	for _, change := range []string{"cwd", "task", "agent", "arguments", "missing", "version", "truncated", "oversize", "symlink"} {
		t.Run(change, func(t *testing.T) {
			p, call := recordedPatch(t)
			if _, err := p.Execute(context.Background(), call); err != nil {
				t.Fatal(err)
			}
			path, _ := p.recordPath(call)
			text, _ := patchText(call)
			switch change {
			case "cwd":
				call.Cwd = t.TempDir()
			case "task":
				call.TaskID = "different"
			case "agent":
				call.AgentID = "different"
			case "arguments":
				call.Arguments, _ = json.Marshal(map[string]string{"patch": text + "\n"})
			case "missing":
				call.ID = "different"
			case "version":
				r, err := checkedPatchRecord(path, call, text)
				if err != nil {
					t.Fatal(err)
				}
				r.Version = 2
				if err := savePatchRecord(path, r); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.WriteFile(path, []byte(`{"version":1`), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, []byte(strings.Repeat("x", maxPatchRecord+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			p.apply = func(context.Context, string, string) (patch.Report, error) { panic("replayed") }
			if _, err := p.Recover(context.Background(), call); patchErrorKind(err) != "operation_unknown" {
				t.Fatal(err)
			}
			if change != "missing" {
				if _, err := p.Execute(context.Background(), call); patchErrorKind(err) != "operation_unknown" {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPatchRecoveryDoesNotCreateJournal(t *testing.T) {
	p, call := recordedPatch(t)
	if _, err := p.Recover(context.Background(), call); patchErrorKind(err) != "operation_unknown" {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery wrote journal", err)
	}
	call.ID = "../escape"
	if _, err := p.Execute(context.Background(), call); patchErrorKind(err) != "invalid_params" {
		t.Fatal(err)
	}
}
