package process

import (
	"context"
	"os"
	"time"

	"zende.local/harness/internal/protocol"
)

// Service is the runner's local IPC endpoint. Core reconnects to this process;
// closing a client connection has no effect on managed command lifetimes.
type Service struct{ Manager *Manager }

type Target struct {
	ID string `json:"process_id"`
}
type WaitParams struct {
	ID           string `json:"process_id"`
	Milliseconds int    `json:"wait_ms"`
}
type ReadParams struct {
	ID     string `json:"process_id"`
	Stream string `json:"stream"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

func (s Service) Handle(ctx context.Context, req protocol.Request) (any, error) {
	switch req.Method {
	case "runner.status":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return struct {
			Instance     string `json:"instance_id"`
			PID          int    `json:"pid"`
			MaxProcesses int    `json:"max_processes"`
			MaxOutput    int64  `json:"max_output_bytes"`
		}{s.Manager.instance, os.Getpid(), s.Manager.maxProcesses, s.Manager.maxOutput}, nil
	case "process.start":
		var p Spec
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.OperationID == "" || req.OperationID != p.ID {
			return nil, protocol.E("invalid_params", "operation_id must equal the stable process_id")
		}
		return s.Manager.Start(ctx, p)
	case "process.inspect":
		var p Target
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Inspect(ctx, p.ID)
	case "process.list":
		var p ListParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.List(ctx, p)
	case "process.wait":
		var p WaitParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Wait(ctx, p.ID, time.Duration(p.Milliseconds)*time.Millisecond)
	case "process.output":
		var p ReadParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Read(ctx, p.ID, p.Stream, p.Offset, p.Limit)
	case "process.stdin":
		var p InputParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Input(ctx, req.OperationID, p)
	case "process.resize":
		var p ResizeParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Resize(ctx, req.OperationID, p)
	case "process.signal":
		var p SignalParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Signal(ctx, req.OperationID, p)
	case "process.control.inspect":
		var p ControlTarget
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.ControlInspect(ctx, p)
	case "process.output.reference":
		var p ReferenceParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Reference(ctx, req.OperationID, p)
	case "process.output.retention":
		var p struct{}
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.Retention(ctx)
	case "process.output.rotate", "process.output.rotation.plan":
		var p RotateParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		if req.Method == "process.output.rotation.plan" {
			return s.Manager.PlanRotation(ctx, p)
		}
		return s.Manager.Rotate(ctx, req.OperationID, p)
	case "process.output.rotation.inspect":
		var p RotationTargetParams
		if err := protocol.StrictDecode(req.Params, &p); err != nil {
			return nil, err
		}
		return s.Manager.RotationInspect(ctx, p.OperationID)
	default:
		return nil, protocol.E("method_not_found", "unknown runner method")
	}
}
func (Service) Subscribe(context.Context, protocol.Request, func(protocol.Event) error) error {
	return protocol.E("method_not_found", "runner output is read with byte cursors")
}

// Remote is a reconnectable handle; it does not own the runner process.
type Remote struct{ Client protocol.Client }

func (r Remote) List(ctx context.Context, p ListParams) (ListPage, error) {
	req, err := protocol.NewRequest("process.list", "", p)
	var page ListPage
	if err == nil {
		err = r.Client.Retry(ctx, req, &page, 3, 100*time.Millisecond)
	}
	return page, err
}

func (r Remote) Start(ctx context.Context, p Spec) (State, error) {
	req, err := protocol.NewRequest("process.start", p.ID, p)
	if err != nil {
		return State{}, err
	}
	var state State
	err = r.Client.Retry(ctx, req, &state, 3, 100*time.Millisecond)
	return state, err
}
func (r Remote) Inspect(ctx context.Context, id string) (State, error) {
	req, _ := protocol.NewRequest("process.inspect", "", Target{ID: id})
	var state State
	err := r.Client.Call(ctx, req, &state)
	return state, err
}

func (r Remote) Wait(ctx context.Context, id string, wait time.Duration) (State, error) {
	if wait < 0 || wait > 30*time.Second {
		return State{}, protocol.E("invalid_params", "wait must be between zero and 30 seconds")
	}
	req, _ := protocol.NewRequest("process.wait", "", WaitParams{ID: id, Milliseconds: int(wait / time.Millisecond)})
	var state State
	err := r.Client.Call(ctx, req, &state)
	return state, err
}
func (r Remote) Read(ctx context.Context, id, stream string, offset int64, limit int) (Output, error) {
	req, _ := protocol.NewRequest("process.output", "", ReadParams{ID: id, Stream: stream, Offset: offset, Limit: limit})
	var output Output
	err := r.Client.Call(ctx, req, &output)
	return output, err
}
func (r Remote) ControlInspect(ctx context.Context, target ControlTarget) (ControlRecord, error) {
	req, err := protocol.NewRequest("process.control.inspect", "", target)
	if err != nil {
		return ControlRecord{}, err
	}
	var record ControlRecord
	err = r.Client.Call(ctx, req, &record)
	return record, err
}

func (r Remote) Resize(ctx context.Context, op string, params ResizeParams) (ControlResult, error) {
	req, err := protocol.NewRequest("process.resize", op, params)
	if err != nil {
		return ControlResult{}, err
	}
	var result ControlResult
	err = r.Client.Retry(ctx, req, &result, 3, 100*time.Millisecond)
	return result, err
}
func (r Remote) Input(ctx context.Context, op string, params InputParams) (ControlResult, error) {
	req, _ := protocol.NewRequest("process.stdin", op, params)
	var result ControlResult
	err := r.Client.Retry(ctx, req, &result, 3, 100*time.Millisecond)
	if err != nil {
		// JSON-RPC errors cannot also carry a result. Query the durable record
		// for a known partial write, without sending the input a second time.
		query, _ := protocol.NewRequest("process.control.inspect", "", ControlTarget{ID: params.ID, OperationID: op})
		var record ControlRecord
		fingerprint, _ := req.Fingerprint()
		if r.Client.Call(ctx, query, &record) == nil && record.Version == 1 && record.OperationID == op && record.Fingerprint == fingerprint {
			result = record.Result
			if record.Status == "completed" {
				if record.Error != nil {
					return result, record.Error
				}
				return result, nil
			}
		}
	}
	return result, err
}

var _ Controller = Remote{}
var _ Controller = (*Manager)(nil)
var _ ControlInspector = Remote{}
var _ ControlInspector = (*Manager)(nil)
