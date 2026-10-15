package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
)

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
	ParentID          string         `json:"parent_id"`
	Input             string         `json:"input"`
	Context           string         `json:"context,omitempty"`
	Cwd               string         `json:"cwd,omitempty"`
	Caller            *Caller        `json:"caller,omitempty"`
	ContextBudget     int            `json:"context_budget,omitempty"`
	Background        string         `json:"background,omitempty"`
	BackgroundHistory []ContextRange `json:"background_history,omitempty"`
}

// ContextRange references immutable context.message events, including endpoints.
type ContextRange struct {
	AgentID string `json:"agent_id"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
}

// An omitted mode with a budget selects recent complete turns. Explicit modes
// are promises: exceeding their budget is an error, not permission to drop data.
func SpawnContextMode(mode string, budget int) (string, int, error) {
	if budget < 0 {
		return "", 0, E("invalid_params", "context budget must be positive or omitted")
	}
	if mode == "" {
		if budget > 0 {
			return "budget", 0, nil
		}
		return "all", 0, nil
	}
	if mode == "all" || mode == "none" {
		return mode, 0, nil
	}
	if strings.HasPrefix(mode, "last:") {
		n, err := strconv.Atoi(strings.TrimPrefix(mode, "last:"))
		if err == nil && n > 0 && mode == "last:"+strconv.Itoa(n) {
			return mode, n, nil
		}
	}
	return "", 0, E("invalid_params", "context must be all, none or last:N (N > 0)")
}

// SpawnSource identifies the actual complete context range selected at the
// creation transaction. Parent history and unfinished work remain untouched.
type SpawnSource struct {
	ParentID           string            `json:"parent_id"`
	ParentCursor       int64             `json:"parent_cursor"`
	Context            string            `json:"context"`
	First              int64             `json:"first,omitempty"`
	Last               int64             `json:"last,omitempty"`
	Messages           int               `json:"messages"`
	ExcludedPending    int               `json:"excluded_pending_messages"`
	Caller             *Caller           `json:"caller,omitempty"`
	RequestID          string            `json:"request_id,omitempty"`
	CallID             string            `json:"call_id,omitempty"`
	Images             map[string]string `json:"images,omitempty"`
	ViewID             string            `json:"view_id,omitempty"`
	SummaryIncluded    bool              `json:"summary_included,omitempty"`
	Turns              int               `json:"turns"`
	ExcludedSelection  int               `json:"excluded_selection_messages"`
	Budget             int               `json:"budget,omitempty"`
	EstimatedTokens    int               `json:"estimated_tokens"`
	EstimateMethod     string            `json:"estimate_method"`
	BackgroundMessages int               `json:"background_messages"`
	BackgroundHistory  []ContextRange    `json:"background_history,omitempty"`
}
