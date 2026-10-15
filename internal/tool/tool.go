// Package tool owns tool descriptors and invocation contracts.
package tool

import (
	"context"
	"encoding/json"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type Descriptor struct {
	model.Tool
	Snippet    string
	Guidelines []string
	Version    string
}

type Call struct {
	ID, AgentID, TaskID, Cwd string
	Arguments                json.RawMessage
}

type Result struct {
	Content   []model.Block `json:"content"`
	ProcessID string        `json:"process_id,omitempty"`
}

type Executor interface {
	Descriptor() Descriptor
	Execute(context.Context, Call) (Result, error)
}

// Recoverer verifies an already dispatched call using its original identity.
// It must never execute the call again. The caller must retain the original
// tool version. An error means the outcome is still unverified; Recovered.Error
// is instead a verified tool failure, which can be committed as a tool result.
type Recoverer interface {
	Recover(context.Context, Call) (Recovered, error)
}

type Recovered struct {
	Result Result
	Error  *protocol.Error
}
