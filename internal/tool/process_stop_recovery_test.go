package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"zende.local/harness/internal/process"
)

type stoppedObservation struct {
	queryOnlyRunner
	state *process.State
}

func (q stoppedObservation) Wait(context.Context, string, time.Duration) (process.State, error) {
	return *q.state, nil
}

func stoppedProof(t *testing.T, m *process.Manager, call Call) process.StopRecord {
	t.Helper()
	ctx := context.Background()
	scope := process.StopScope{Agents: []string{call.AgentID}, Pending: []process.StopDispatch{{ID: call.ID, AgentID: call.AgentID, TaskID: call.TaskID}}}
	r, err := m.FreezeStop(ctx, "tree-stop", scope)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); r.State == "frozen" && time.Now().Before(deadline); {
		r, err = m.AdvanceStop(ctx, "tree-stop")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.State != "stopped" {
		t.Fatal(r)
	}
	return r
}

func TestRecoverStoppedUndeliveredExecNeverDispatchesOrInspects(t *testing.T) {
	m, _ := toolRunner(t)
	call := Call{ID: "delayed", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Arguments: json.RawMessage(`{"command":"touch marker"}`)}
	proof := stoppedProof(t, m, call)
	q := queryOnlyRunner{inspect: func(context.Context, string) (process.State, error) { panic("not-started recovery queried a process") }}
	r, err := (processTool{runner: q}).RecoverStopped(context.Background(), call, proof)
	if err != nil || r.Error == nil || r.Error.Data.Kind != "task_cancelled" {
		t.Fatal(r, err)
	}
	page := recoveredPage(t, r)
	if page.Status != "not_started" || page.Stop == nil || page.Stop.OperationID != proof.OperationID || page.Stop.State != "not_started" || page.ExitCode != nil || len(page.Output) != 0 {
		t.Fatal(page)
	}
	if _, err := m.ReleaseStop(context.Background(), proof.OperationID); err != nil {
		t.Fatal(err)
	}
	spec, _, _, err := prepareExec(call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), spec); err == nil {
		t.Fatal("delayed RPC executed after release")
	}
}

func TestRecoverStoppedRetainsUnknownExitAndAvailableOutput(t *testing.T) {
	ctx := context.Background()
	m, _ := toolRunner(t)
	call := Call{ID: "original", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Arguments: json.RawMessage(`{"command":"printf retained; read value","wait_ms":0}`)}
	spec, _, _, err := prepareExec(call)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Signal(ctx, "cleanup", process.SignalParams{ID: call.ID, Signal: "KILL"}) })
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		out, err := m.Read(ctx, call.ID, "stdout", 0, 1024)
		if err == nil && out.Text == "retained" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	proof := stoppedProof(t, m, call)
	state.Status = "lost"
	state.ExitCode = nil
	q := stoppedObservation{queryOnlyRunner: queryOnlyRunner{Controller: m, inspect: func(context.Context, string) (process.State, error) { return state, nil }}, state: &state}
	r, err := (processTool{runner: q}).RecoverStopped(ctx, call, proof)
	if err != nil || r.Error == nil || r.Error.Data.Kind != "task_cancelled" {
		t.Fatal(r, err)
	}
	page := recoveredPage(t, r)
	if page.Status != "lost" || page.ExitCode != nil || page.Stop == nil || page.Stop.State != "stopped" || !page.ObservationInterrupted || len(page.Output) < 1 || page.Output[0].Text != "retained" {
		t.Fatal(page)
	}
	state.Command = "different"
	if _, err := (processTool{runner: q}).RecoverStopped(ctx, call, proof); err == nil {
		t.Fatal("different original command accepted")
	}
	state.Spec = spec
	state.PID++
	if _, err := (processTool{runner: q}).RecoverStopped(ctx, call, proof); err == nil {
		t.Fatal("different original PID accepted")
	}
}

func TestRecoverStoppedRejectsIncompleteProofAndInput(t *testing.T) {
	m, _ := toolRunner(t)
	call := Call{ID: "original", AgentID: "agent", TaskID: "task", Cwd: t.TempDir(), Arguments: json.RawMessage(`{"command":"true"}`)}
	proof := stoppedProof(t, m, call)
	q := queryOnlyRunner{Controller: m}
	for _, scenario := range []string{"frozen", "missing-target", "fingerprint", "wrong-agent", "wrong-task", "input"} {
		t.Run(scenario, func(t *testing.T) {
			p, c := proof, call
			executor := processTool{runner: q}
			switch scenario {
			case "frozen":
				p.State = "frozen"
			case "missing-target":
				p.Processes = nil
			case "fingerprint":
				p.Fingerprint = "wrong"
			case "wrong-agent":
				c.AgentID = "other"
			case "wrong-task":
				c.TaskID = "other"
			case "input":
				executor.input = true
			}
			if _, err := executor.RecoverStopped(context.Background(), c, p); err == nil {
				t.Fatal("unverified cancellation accepted")
			}
		})
	}
}

var _ StoppedRecoverer = processTool{}
