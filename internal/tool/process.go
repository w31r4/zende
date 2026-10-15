package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

const DefaultWaitMS = 1000
const DefaultReadBytes = 32768
const MaxReadBytes = 65536

type execParams struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
	PTY       bool   `json:"pty"`
	Cols      *int   `json:"cols"`
	Rows      *int   `json:"rows"`
	WaitMS    *int   `json:"wait_ms"`
	MaxOutput *int   `json:"max_output_bytes"`
}
type inputParams struct {
	ID        string           `json:"process_id"`
	Chars     string           `json:"chars"`
	Close     bool             `json:"close_stdin"`
	Offsets   map[string]int64 `json:"offsets"`
	WaitMS    *int             `json:"wait_ms"`
	MaxOutput *int             `json:"max_output_bytes"`
}

// ProcessPage is a bounded, display-safe model projection. CLI output retains
// raw bytes separately; byte cursors here still refer to those original bytes.
type ProcessPage struct {
	Stream string       `json:"stream"`
	Text   string       `json:"text"`
	Next   int64        `json:"next"`
	Total  int64        `json:"total"`
	Gap    *process.Gap `json:"gap,omitempty"`
}
type ProcessResult struct {
	ID                     string        `json:"process_id"`
	Status                 string        `json:"status"`
	PTY                    bool          `json:"pty"`
	StdinOpen              bool          `json:"stdin_open"`
	ExitCode               *int          `json:"exit_code,omitempty"`
	ExitSignal             string        `json:"exit_signal,omitempty"`
	Failure                string        `json:"failure,omitempty"`
	Written                *int          `json:"written,omitempty"`
	Output                 []ProcessPage `json:"output"`
	ObservationInterrupted bool          `json:"observation_interrupted,omitempty"`
	Stop                   *ProcessStop  `json:"stop,omitempty"`
}

type ProcessStop struct {
	OperationID string `json:"operation_id"`
	State       string `json:"state"` // stopped or not_started, never an exit code
}

type processTool struct {
	runner process.Controller
	input  bool
}

func ProcessExecutors(runner process.Controller) []Executor {
	return []Executor{processTool{runner: runner}, processTool{runner: runner, input: true}}
}

func (t processTool) Descriptor() Descriptor {
	d := Descriptor{Version: "2", Guidelines: []string{
		"Keep process_id and each stream's next byte cursor. Use write_stdin to continue the same process; do not re-run its command to wait for output.",
		"wait_ms bounds waiting, not command lifetime. Empty output does not mean the process exited. Check status and exit_code.",
		"Output text escapes terminal controls. next counts original bytes, not displayed characters. Read more with offsets; gap explicitly identifies unavailable bytes.",
		"Stopping a task stops observation, not an accepted background process. observation_interrupted marks the last observed state; inspect process_id before continuing.",
	}}
	if !t.input {
		d.Tool = model.Tool{Name: "exec_command", Description: "Run a command with the configured shell. cwd defaults to the Agent directory; relative cwd resolves there. pipe is default, pty=true uses a real tmux terminal. Returns process_id, status, exit result and stdout/stderr or combined pty pages. Wait defaults to 1000 ms; output defaults to 32768 bytes per stream. Long commands continue after return.", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1},"cwd":{"type":"string"},"pty":{"type":"boolean","default":false},"cols":{"type":"integer","minimum":2,"maximum":1000},"rows":{"type":"integer","minimum":2,"maximum":1000},"wait_ms":{"type":"integer","minimum":0,"maximum":30000,"default":1000},"max_output_bytes":{"type":"integer","minimum":1,"maximum":65536,"default":32768}},"required":["command"],"additionalProperties":false}`)}
		d.Snippet = "Execute shell commands, searches, tests and generators; retain long-running process identities."
	} else {
		d.Tool = model.Tool{Name: "write_stdin", Description: "Continue an existing process, optionally sending chars or closing stdin. Omit chars and close_stdin for a read-only wait. No newline is added; chars is limited to 64 KiB UTF-8 bytes. offsets selects each stream's original byte position (default 0). Returns written bytes, status and output pages. Pipe close sends EOF; PTY close requires canonical mode with EOF enabled. Wait defaults to 1000 ms and never kills the process.", InputSchema: json.RawMessage(`{"type":"object","properties":{"process_id":{"type":"string","minLength":1},"chars":{"type":"string"},"close_stdin":{"type":"boolean","default":false},"offsets":{"type":"object","properties":{"stdout":{"type":"integer","minimum":0},"stderr":{"type":"integer","minimum":0},"pty":{"type":"integer","minimum":0}},"additionalProperties":false},"wait_ms":{"type":"integer","minimum":0,"maximum":30000,"default":1000},"max_output_bytes":{"type":"integer","minimum":1,"maximum":65536,"default":32768}},"required":["process_id"],"additionalProperties":false}`)}
		d.Snippet = "Send input or read output from an existing process."
		d.Guidelines = append(d.Guidelines, "On an input error, inspect written and the original control operation before retrying. A timeout may have applied input; never resend it under a new operation ID blindly.")
	}
	return d
}

func decodeArguments(raw json.RawMessage, target any, keys ...string) error {
	var fields map[string]json.RawMessage
	if protocol.StrictDecode(raw, &fields) != nil || fields == nil {
		return protocol.E("invalid_params", "tool arguments must be a JSON object")
	}
	for key, value := range fields {
		known := false
		for _, allowed := range keys {
			known = known || key == allowed
		}
		var decoded any
		if !known || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &decoded) != nil || containsNull(decoded) {
			return protocol.E("invalid_params", "unknown or null tool argument")
		}
	}
	return protocol.StrictDecode(raw, target)
}

func containsNull(value any) bool {
	if value == nil {
		return true
	}
	switch v := value.(type) {
	case map[string]any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	}
	return false
}

func processOptions(wait, limit *int) (time.Duration, int, error) {
	w, n := DefaultWaitMS, DefaultReadBytes
	if wait != nil {
		w = *wait
	}
	if limit != nil {
		n = *limit
	}
	if w < 0 || w > 30000 || n < 1 || n > MaxReadBytes {
		return 0, 0, protocol.E("invalid_params", "wait_ms must be 0–30000 and max_output_bytes 1–65536")
	}
	return time.Duration(w) * time.Millisecond, n, nil
}

func (t processTool) Execute(ctx context.Context, call Call) (Result, error) {
	if t.runner == nil {
		return Result{}, protocol.E("unavailable", "runner is not configured")
	}
	if t.input {
		return t.continueProcess(ctx, call)
	}
	spec, wait, limit, err := prepareExec(call)
	if err != nil {
		return Result{}, err
	}
	acceptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	state, err := t.runner.Start(acceptCtx, spec)
	cancel()
	if state.ID == "" {
		state = process.State{Spec: process.Spec{ID: call.ID, PTY: spec.PTY}, Status: "unknown"}
	}
	if err != nil {
		return processResult(state, nil, nil), err
	}
	return t.observe(ctx, state, nil, nil, wait, limit, true)
}

func prepareExec(call Call) (process.Spec, time.Duration, int, error) {
	var p execParams
	if err := decodeArguments(call.Arguments, &p, "command", "cwd", "pty", "cols", "rows", "wait_ms", "max_output_bytes"); err != nil {
		return process.Spec{}, 0, 0, err
	}
	wait, limit, err := processOptions(p.WaitMS, p.MaxOutput)
	if err != nil {
		return process.Spec{}, 0, 0, err
	}
	if p.Command == "" {
		return process.Spec{}, 0, 0, protocol.E("invalid_params", "command is required")
	}
	cols, rows := 0, 0
	if p.Cols != nil {
		cols = *p.Cols
	}
	if p.Rows != nil {
		rows = *p.Rows
	}
	if !p.PTY && (p.Cols != nil || p.Rows != nil) || p.PTY && (p.Cols != nil && (cols < 2 || cols > 1000) || p.Rows != nil && (rows < 2 || rows > 1000)) {
		return process.Spec{}, 0, 0, protocol.E("invalid_params", "cols and rows apply only to PTY and must be 2–1000")
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = call.Cwd
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(call.Cwd, cwd)
	}
	return process.Spec{ID: call.ID, AgentID: call.AgentID, TaskID: call.TaskID, Cwd: cwd, Command: p.Command, PTY: p.PTY, Cols: cols, Rows: rows}, wait, limit, nil
}

func prepareInput(call Call) (inputParams, time.Duration, int, error) {
	var p inputParams
	if err := decodeArguments(call.Arguments, &p, "process_id", "chars", "close_stdin", "offsets", "wait_ms", "max_output_bytes"); err != nil {
		return p, 0, 0, err
	}
	wait, limit, err := processOptions(p.WaitMS, p.MaxOutput)
	if err != nil {
		return p, 0, 0, err
	}
	if p.ID == "" || len(p.Chars) > process.MaxInputBytes {
		return p, 0, 0, protocol.E("invalid_params", "process_id is required; chars must fit 64 KiB")
	}
	return p, wait, limit, nil
}

func (t processTool) continueProcess(ctx context.Context, call Call) (Result, error) {
	p, wait, limit, err := prepareInput(call)
	if err != nil {
		return Result{}, err
	}
	query, cancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := t.runner.Inspect(query, p.ID)
	cancel()
	if err != nil {
		return Result{}, err
	}
	for stream, offset := range p.Offsets {
		saved, ok := state.Output[stream]
		if !ok || offset < 0 || offset > saved.Total {
			return processResult(state, nil, nil), protocol.E("invalid_params", "offsets must select existing streams and available byte positions")
		}
	}
	mutation := p.Chars != "" || p.Close
	var written *int
	if mutation {
		inputCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		input, err := t.runner.Input(inputCtx, call.ID, process.InputParams{ID: p.ID, Data: []byte(p.Chars), Close: p.Close})
		cancel()
		if input.OperationID != "" {
			n := input.Written
			written = &n
		}
		if input.State.ID != "" {
			state = input.State
		}
		if err != nil {
			return processResult(state, written, nil), err
		}
	}
	return t.observe(ctx, state, written, p.Offsets, wait, limit, mutation)
}

func processResult(state process.State, written *int, output []ProcessPage) Result {
	return processProjection(state, written, output, false)
}

func processProjection(state process.State, written *int, output []ProcessPage, interrupted bool) Result {
	if output == nil {
		output = []ProcessPage{}
	}
	value := ProcessResult{ID: state.ID, Status: state.Status, PTY: state.PTY, StdinOpen: state.StdinOpen, ExitCode: state.ExitCode, ExitSignal: state.ExitSignal, Failure: state.Failure, Written: written, Output: output, ObservationInterrupted: interrupted}
	b, _ := json.Marshal(value)
	return Result{ProcessID: state.ID, Content: []model.Block{{Type: "text", Text: string(b)}}}
}

func (t processTool) observe(ctx context.Context, state process.State, written *int, offsets map[string]int64, wait time.Duration, limit int, mutation bool) (Result, error) {
	failure := func(err error, output []ProcessPage) (Result, error) {
		var known *protocol.Error
		transportInterrupted := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if errors.As(err, &known) {
			transportInterrupted = known.Data.Kind == "transport_unknown" || known.Data.Kind == "unavailable"
		}
		if ctx.Err() != nil && transportInterrupted {
			// Start/Input acceptance has already been confirmed. An intentional
			// stop of this read-only wait does not make that mutation unknown.
			return processProjection(state, written, output, true), nil
		}
		if mutation {
			err = protocol.E("operation_unknown", "process operation was accepted but observation failed; inspect the original process and operation before retrying")
		}
		return processResult(state, written, output), err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait+5*time.Second)
	current, err := t.runner.Wait(waitCtx, state.ID, wait)
	cancel()
	if err != nil {
		return failure(err, nil)
	}
	state = current
	streams := []string{"stdout", "stderr"}
	if state.PTY {
		streams = []string{"pty"}
	}
	pages := []ProcessPage{}
	for _, stream := range streams {
		query, cancel := context.WithTimeout(ctx, 5*time.Second)
		output, err := t.runner.Read(query, state.ID, stream, offsets[stream], limit)
		cancel()
		if err != nil {
			return failure(err, pages)
		}
		pages = append(pages, ProcessPage{Stream: stream, Text: output.Text, Next: output.Next, Total: output.Total, Gap: output.Gap})
	}
	return processResult(state, written, pages), nil
}
