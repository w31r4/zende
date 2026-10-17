// Package process owns runner-managed processes and raw output. It never writes UI output.
package process

import (
	"context"
	"time"
)

type Spec struct {
	ID      string `json:"process_id"`
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	PTY     bool   `json:"pty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
}

type State struct {
	Spec
	SchemaVersion int                        `json:"schema_version"`
	Configuration *Configuration             `json:"configuration,omitempty"`
	InstanceID    string                     `json:"instance_id"`
	Status        string                     `json:"status"`
	ExitCode      *int                       `json:"exit_code,omitempty"`
	ExitSignal    string                     `json:"exit_signal,omitempty"`
	Terminal      *Terminal                  `json:"terminal,omitempty"`
	PID           int                        `json:"pid,omitempty"`
	Identity      string                     `json:"identity,omitempty"`
	StdinOpen     bool                       `json:"stdin_open"`
	InputBytes    int64                      `json:"input_bytes"`
	CancelSignal  string                     `json:"cancel_signal,omitempty"`
	CreatedAt     int64                      `json:"created_at,omitempty"`
	References    map[string]OutputReference `json:"output_references,omitempty"`
	ReleasedBy    string                     `json:"output_released_by,omitempty"`
	Output        map[string]StreamState     `json:"output"`
	Failure       string                     `json:"failure,omitempty"`
}

// Terminal binds a pane to a particular server lifetime, not merely its name.
type Terminal struct {
	Socket         string `json:"socket"`
	Session        string `json:"session"`
	Pane           string `json:"pane"`
	ServerPID      int    `json:"server_pid"`
	ServerIdentity string `json:"server_identity"`
	Token          string `json:"token"`
	Cols           int    `json:"cols"`
	Rows           int    `json:"rows"`
}

type StreamState struct {
	Stored int64 `json:"stored"`
	Total  int64 `json:"total"`
}
type Gap struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}
type Output struct {
	ProcessID string `json:"process_id"`
	Stream    string `json:"stream"`
	Data      []byte `json:"data"`
	Text      string `json:"text"`
	Next      int64  `json:"next"`
	Total     int64  `json:"total"`
	Gap       *Gap   `json:"gap,omitempty"`
	Status    string `json:"status"`
}

type Runner interface {
	Start(context.Context, Spec) (State, error)
	Inspect(context.Context, string) (State, error)
}

// Controller is the model-tool boundary to the independent runner. The smaller
// Runner interface remains useful to read-only core consumers.
type Controller interface {
	Runner
	Wait(context.Context, string, time.Duration) (State, error)
	Read(context.Context, string, string, int64, int) (Output, error)
	Input(context.Context, string, InputParams) (ControlResult, error)
}

// ControlInspector queries persisted input effects without dispatching them.
type ControlInspector interface {
	ControlInspect(context.Context, ControlTarget) (ControlRecord, error)
}
