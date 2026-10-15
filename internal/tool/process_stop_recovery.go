package tool

import (
	"context"
	"encoding/json"
	"slices"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

// StoppedRecoverer is used only for explicit tree cancellation. Other unknown
// side effects (including stdin delivery and patch) retain their verifier gate.
type StoppedRecoverer interface {
	RecoverStopped(context.Context, Call, process.StopRecord) (Recovered, error)
}

func (t processTool) RecoverStopped(ctx context.Context, call Call, proof process.StopRecord) (Recovered, error) {
	if t.input || t.runner == nil {
		return Recovered{}, unresolvedProcess()
	}
	if err := proof.Validate(); err != nil {
		return Recovered{}, err
	}
	if proof.State != "stopped" && proof.State != "released" {
		return Recovered{}, unresolvedProcess()
	}
	dispatch := process.StopDispatch{ID: call.ID, AgentID: call.AgentID, TaskID: call.TaskID}
	if !slices.Contains(proof.Scope.Pending, dispatch) {
		return Recovered{}, unresolvedProcess()
	}
	var target *process.StopProcess
	for i := range proof.Processes {
		if proof.Processes[i].StopDispatch == dispatch {
			target = &proof.Processes[i]
			break
		}
	}
	if target == nil {
		return Recovered{}, unresolvedProcess()
	}
	spec, _, limit, err := prepareExec(call)
	if err != nil {
		return Recovered{}, err
	}
	if spec.PTY {
		if spec.Cols == 0 {
			spec.Cols = 80
		}
		if spec.Rows == 0 {
			spec.Rows = 24
		}
	}
	value := ProcessResult{ID: call.ID, PTY: spec.PTY, Output: []ProcessPage{}, Stop: &ProcessStop{proof.OperationID, target.State}}
	if target.State == "not_started" {
		value.Status = "not_started"
	} else if target.State == "stopped" {
		state, err := t.runner.Inspect(ctx, call.ID)
		if err != nil {
			return Recovered{}, unresolvedProcess()
		}
		if state.SchemaVersion != 1 || state.Spec != spec || state.PID != target.PID || state.Identity != target.Identity || state.PTY != target.PTY {
			return Recovered{}, protocol.E("operation_conflict", "stopped process does not match the original tool call")
		}
		observed, readErr := t.observe(ctx, state, nil, nil, 0, limit, false)
		if len(observed.Content) != 1 || json.Unmarshal([]byte(observed.Content[0].Text), &value) != nil {
			return Recovered{}, unresolvedProcess()
		}
		value.Stop = &ProcessStop{proof.OperationID, target.State}
		value.ObservationInterrupted = true
		if readErr != nil {
			value.Failure = "Saved output could not be fully read; inspect the original process and output files."
		}
	} else {
		return Recovered{}, unresolvedProcess()
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Recovered{}, err
	}
	return Recovered{Result: Result{ProcessID: call.ID, Content: []model.Block{{Type: "text", Text: string(data)}}}, Error: protocol.E("task_cancelled", "tree stop verified process termination or prevented start; prior effects remain, unknown exit status is not success")}, nil
}
