package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

// Mutations panic even when the underlying runner would deduplicate them.
// Recovery must be read-only, not merely end with the same external state.
type queryOnlyRunner struct {
	process.Controller
	inspect func(context.Context, string) (process.State, error)
	control func(context.Context, process.ControlTarget) (process.ControlRecord, error)
}

func (q queryOnlyRunner) Start(context.Context, process.Spec) (process.State, error) {
	panic("recovery dispatched a command")
}
func (q queryOnlyRunner) Input(context.Context, string, process.InputParams) (process.ControlResult, error) {
	panic("recovery dispatched input")
}
func (q queryOnlyRunner) Inspect(ctx context.Context, id string) (process.State, error) {
	if q.inspect != nil {
		return q.inspect(ctx, id)
	}
	return q.Controller.Inspect(ctx, id)
}
func (q queryOnlyRunner) ControlInspect(ctx context.Context, target process.ControlTarget) (process.ControlRecord, error) {
	if q.control != nil {
		return q.control(ctx, target)
	}
	return q.Controller.(process.ControlInspector).ControlInspect(ctx, target)
}

func recoveredPage(t *testing.T, r Recovered) ProcessResult {
	t.Helper()
	var page ProcessResult
	if len(r.Result.Content) != 1 || json.Unmarshal([]byte(r.Result.Content[0].Text), &page) != nil {
		t.Fatal(r)
	}
	return page
}

func TestRecoverProcessAndInputWithoutReplayingEffects(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(strconv.FormatBool(pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("real tmux required")
				}
			}
			m, registry := toolRunner(t)
			cwd := t.TempDir()
			start := Call{ID: "start-original", AgentID: "agent", TaskID: "task", Cwd: cwd, Arguments: arguments(t, map[string]any{"command": "printf x >> marker; read value; printf '%s' \"$value\" >> input-log", "pty": pty, "wait_ms": 0})}
			page, err := executeProcessTool(t, m, registry, "exec_command", start)
			if err != nil || page.Status != "running" {
				t.Fatal(page, err)
			}
			q := queryOnlyRunner{Controller: m}
			starter := processTool{runner: q}
			for range 2 {
				recovered, err := starter.Recover(context.Background(), start)
				if err != nil || recovered.Error != nil || recoveredPage(t, recovered).Status != "running" {
					t.Fatal(recovered, err)
				}
			}
			input := Call{ID: "input-original", Arguments: arguments(t, map[string]any{"process_id": start.ID, "chars": "hello\n", "wait_ms": 3000})}
			page, err = executeProcessTool(t, m, registry, "write_stdin", input)
			if err != nil || page.Status != "exited" {
				t.Fatal(page, err)
			}
			for range 2 {
				recovered, err := (processTool{runner: q, input: true}).Recover(context.Background(), input)
				if err != nil || recovered.Error != nil {
					t.Fatal(recovered, err)
				}
				value := recoveredPage(t, recovered)
				if value.Written == nil || *value.Written != 6 || value.Status != "exited" {
					t.Fatal(value)
				}
				recovered, err = starter.Recover(context.Background(), start)
				if err != nil || recoveredPage(t, recovered).Status != "exited" {
					t.Fatal(recovered, err)
				}
			}
			for file, want := range map[string]string{"marker": "x", "input-log": "hello"} {
				got, err := os.ReadFile(filepath.Join(cwd, file))
				if err != nil || string(got) != want {
					t.Fatal(file, string(got), err)
				}
			}
		})
	}
}

func TestRecoverExecRejectsMissingUncertainAndDifferentProcess(t *testing.T) {
	call := Call{ID: "original", AgentID: "agent", TaskID: "task", Cwd: "/fixture", Arguments: json.RawMessage(`{"command":"touch marker"}`)}
	for _, scenario := range []string{"missing", "starting", "unknown", "lost", "agent", "task", "cwd", "command", "id", "schema", "pty"} {
		t.Run(scenario, func(t *testing.T) {
			q := queryOnlyRunner{inspect: func(_ context.Context, id string) (process.State, error) {
				if id != call.ID {
					t.Fatal("inspected replacement identity", id)
				}
				s := process.State{Spec: process.Spec{ID: call.ID, AgentID: call.AgentID, TaskID: call.TaskID, Cwd: call.Cwd, Command: "touch marker"}, SchemaVersion: 1, Status: "running"}
				switch scenario {
				case "missing":
					return process.State{}, protocol.E("not_found", "record missing")
				case "starting", "unknown", "lost":
					s.Status = scenario
				case "agent":
					s.AgentID = "replacement"
				case "task":
					s.TaskID = "replacement"
				case "cwd":
					s.Cwd = "/replacement"
				case "command":
					s.Command = "other command"
				case "id":
					s.ID = "replacement"
				case "schema":
					s.SchemaVersion = 2
				case "pty":
					s.PTY = true
				}
				return s, nil
			}}
			if _, err := (processTool{runner: q}).Recover(context.Background(), call); err == nil {
				t.Fatal("unverified effect accepted")
			}
		})
	}
}

func TestRecoverInputRequiresOriginalCompletedControl(t *testing.T) {
	call := Call{ID: "input-original", Arguments: json.RawMessage(`{"process_id":"original","chars":"hello"}`)}
	req, _ := protocol.NewRequest("process.stdin", call.ID, process.InputParams{ID: "original", Data: []byte("hello")})
	fingerprint, _ := req.Fingerprint()
	for _, scenario := range []string{"missing", "pending", "fingerprint", "operation", "result-operation", "result-process", "written", "short-success", "schema", "uncertain-error", "success", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			q := queryOnlyRunner{Controller: unavailableObservation{}, control: func(_ context.Context, target process.ControlTarget) (process.ControlRecord, error) {
				if target.ID != "original" || target.OperationID != call.ID {
					t.Fatal(target)
				}
				r := process.ControlRecord{Version: 1, OperationID: call.ID, Fingerprint: fingerprint, Status: "completed", Result: process.ControlResult{OperationID: call.ID, Written: 5, State: process.State{Spec: process.Spec{ID: "original"}, Status: "running"}}}
				switch scenario {
				case "missing":
					return process.ControlRecord{}, protocol.E("not_found", "missing")
				case "pending":
					r.Status = "pending"
				case "fingerprint":
					r.Fingerprint = "different-input"
				case "operation":
					r.OperationID = "replacement"
				case "result-operation":
					r.Result.OperationID = "replacement"
				case "result-process":
					r.Result.State.ID = "replacement"
				case "written":
					r.Result.Written = 6
				case "short-success":
					r.Result.Written = 2
				case "schema":
					r.Version = 2
				case "uncertain-error":
					r.Error = protocol.E("transport_unknown", "dispatch timed out")
				case "partial":
					r.Result.Written = 2
					r.Error = protocol.E("input_partial", "two bytes written")
				}
				return r, nil
			}}
			r, err := (processTool{runner: q, input: true}).Recover(context.Background(), call)
			if scenario != "success" && scenario != "partial" {
				if err == nil {
					t.Fatal("unverified input accepted", r)
				}
				return
			}
			if err != nil || (scenario == "partial") != (r.Error != nil) {
				t.Fatal(r, err)
			}
			page := recoveredPage(t, r)
			want := 5
			if scenario == "partial" {
				want = 2
			}
			if !page.ObservationInterrupted || page.Written == nil || *page.Written != want {
				t.Fatal(page)
			}
		})
	}
}

type unavailableObservation struct{ process.Controller }

func (unavailableObservation) Wait(context.Context, string, time.Duration) (process.State, error) {
	return process.State{}, errors.New("query interrupted")
}

func TestRecoverReadOnlyCallNeverNeedsControlRecord(t *testing.T) {
	m, registry := toolRunner(t)
	page, err := executeProcessTool(t, m, registry, "exec_command", Call{ID: "readonly", Cwd: t.TempDir(), Arguments: json.RawMessage(`{"command":"printf abc","wait_ms":3000}`)})
	if err != nil || page.Status != "exited" {
		t.Fatal(page, err)
	}
	q := queryOnlyRunner{Controller: m, control: func(context.Context, process.ControlTarget) (process.ControlRecord, error) {
		t.Fatal("read-only call queried a mutation record")
		return process.ControlRecord{}, nil
	}}
	r, err := (processTool{runner: q, input: true}).Recover(context.Background(), Call{ID: "read", Arguments: json.RawMessage(`{"process_id":"readonly","offsets":{"stdout":1},"wait_ms":30000}`)})
	if err != nil || recoveredPage(t, r).Output[0].Text != "bc" {
		t.Fatal(r, err)
	}
}
