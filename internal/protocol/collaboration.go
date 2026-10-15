package protocol

import "encoding/json"

// EffectFingerprint excludes retry provenance only for collaboration writes.
// Explicit request IDs may be retried from a different shell/tool call. The
// first accepted caller remains in history; changing task content still fails.
// Fingerprint itself retains provenance for automatic caller-local IDs.
func (r Request) EffectFingerprint() (string, error) {
	if r.Method == "agent.spawn" || r.Method == "agent.submit" {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(r.Params, &params); err != nil {
			return "", err
		}
		delete(params, "caller")
		raw, err := json.Marshal(params)
		if err != nil {
			return "", err
		}
		r.Params = raw
	}
	return r.Fingerprint()
}

// Caller is execution provenance, not an authorization credential. The runner
// supplies it to shells; core verifies it against a persisted tool dispatch.
type Caller struct {
	AgentID     string `json:"agent_id"`
	TaskID      string `json:"task_id"`
	OperationID string `json:"operation_id"`
}

const CallerEnvironment = "ZENDE_CALLER"

type CallSource struct {
	Caller    Caller `json:"caller"`
	RequestID string `json:"request_id"`
	CallID    string `json:"call_id"`
}

type SpawnAgent struct {
	ParentID string  `json:"parent_id"`
	Input    string  `json:"input"`
	Context  string  `json:"context,omitempty"`
	Cwd      string  `json:"cwd,omitempty"`
	Caller   *Caller `json:"caller,omitempty"`
}

// SpawnSource identifies the actual complete context prefix selected at the
// creation transaction. Parent history and unfinished work remain untouched.
type SpawnSource struct {
	ParentID        string            `json:"parent_id"`
	ParentCursor    int64             `json:"parent_cursor"`
	Context         string            `json:"context"`
	First           int64             `json:"first,omitempty"`
	Last            int64             `json:"last,omitempty"`
	Messages        int               `json:"messages"`
	ExcludedPending int               `json:"excluded_pending_messages"`
	Caller          *Caller           `json:"caller,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	CallID          string            `json:"call_id,omitempty"`
	Images          map[string]string `json:"images,omitempty"`
}
