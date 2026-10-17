package tool

import (
	"context"
	"encoding/json"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func unresolvedProcess() error {
	return protocol.E("operation_unknown", "original process operation is unverified; no command or input was sent")
}

func (t processTool) Recover(ctx context.Context, call Call) (Recovered, error) {
	if t.runner == nil {
		return Recovered{}, protocol.E("unavailable", "runner is not configured")
	}
	// Every operation below is a bounded query. In particular, Start and Input
	// must not be used here even though they accept stable operation identities.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if t.input {
		return t.recoverInput(ctx, call)
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
	state, err := t.runner.Inspect(ctx, call.ID)
	if err != nil {
		return Recovered{}, unresolvedProcess()
	}
	if !state.ValidConfigurationMetadata() || state.Spec != spec {
		return Recovered{}, protocol.E("operation_conflict", "saved process does not match the original tool call")
	}
	if state.Status == "failed" {
		return Recovered{Result: processResult(state, nil, nil), Error: protocol.E("process_start_failed", "command could not start; inspect cwd and shell")}, nil
	}
	if state.Status != "running" && state.Status != "exited" {
		return Recovered{}, unresolvedProcess()
	}
	result, err := t.observe(ctx, state, nil, nil, 0, limit, false)
	if err != nil || !knownObservedProcess(result, call.ID) {
		return Recovered{}, unresolvedProcess()
	}
	return Recovered{Result: result}, nil
}

func knownObservedProcess(result Result, id string) bool {
	var value ProcessResult
	return result.ProcessID == id && len(result.Content) == 1 && json.Unmarshal([]byte(result.Content[0].Text), &value) == nil && value.ID == id && !value.ObservationInterrupted && (value.Status == "running" || value.Status == "exited")
}

func (t processTool) recoverInput(ctx context.Context, call Call) (Recovered, error) {
	p, _, limit, err := prepareInput(call)
	if err != nil {
		return Recovered{}, err
	}
	if p.Chars == "" && !p.Close {
		state, err := t.runner.Inspect(ctx, p.ID)
		if err != nil {
			return Recovered{}, err
		}
		result, err := t.observe(ctx, state, nil, p.Offsets, 0, limit, false)
		return Recovered{Result: result}, err
	}
	inspector, ok := t.runner.(process.ControlInspector)
	if !ok {
		return Recovered{}, protocol.E("unsupported", "runner cannot inspect saved input operations")
	}
	record, err := inspector.ControlInspect(ctx, process.ControlTarget{ID: p.ID, OperationID: call.ID})
	if err != nil {
		return Recovered{}, unresolvedProcess()
	}
	req, err := protocol.NewRequest("process.stdin", call.ID, process.InputParams{ID: p.ID, Data: []byte(p.Chars), Close: p.Close})
	if err != nil {
		return Recovered{}, err
	}
	fingerprint, err := req.Fingerprint()
	if err != nil || record.Version != 1 || record.OperationID != call.ID || record.Fingerprint != fingerprint {
		return Recovered{}, protocol.E("operation_conflict", "saved input operation does not match the original tool call")
	}
	if record.Status != "completed" || record.Error != nil && (record.Error.Data.Kind == "operation_unknown" || record.Error.Data.Kind == "transport_unknown") {
		return Recovered{}, unresolvedProcess()
	}
	if record.Result.OperationID != call.ID || record.Result.Written < 0 || record.Result.Written > len(p.Chars) || record.Result.State.ID != "" && record.Result.State.ID != p.ID || record.Error == nil && (record.Result.State.ID == "" || record.Result.Written != len(p.Chars)) {
		return Recovered{}, protocol.E("operation_conflict", "saved input result is inconsistent")
	}
	state := record.Result.State
	if state.ID == "" {
		state = process.State{Spec: process.Spec{ID: p.ID}, Status: "unknown"}
	}
	// Input effects are already proved by the completed control record. A lost
	// process or unreadable output cannot turn those known effects into another
	// input dispatch. Return the saved state with an interrupted observation.
	written := record.Result.Written
	result, observeErr := t.observe(ctx, state, &written, p.Offsets, 0, limit, false)
	if observeErr != nil {
		result = processProjection(state, &written, nil, true)
	}
	return Recovered{Result: result, Error: record.Error}, nil
}

var _ Recoverer = processTool{}
