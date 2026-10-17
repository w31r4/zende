package protocol

// The policy is captured once per Task. A model change updates the capacity,
// not the Task's compaction preferences. It contains no provider binding data.
type ContextBudgetPolicy struct {
	Version          int `json:"version"`
	TriggerPercent   int `json:"trigger_percent"`
	ReserveTokens    int `json:"reserve_tokens"`
	SummaryTokens    int `json:"summary_tokens"`
	KeepRecentTokens int `json:"keep_recent_tokens"`
	MaxAttempts      int `json:"max_attempts"`
}

func (p ContextBudgetPolicy) Valid() bool {
	return p.Version == 1 && p.TriggerPercent >= 1 && p.TriggerPercent <= 100 &&
		p.ReserveTokens > 0 && p.ReserveTokens <= 1<<30 &&
		p.SummaryTokens > 0 && p.SummaryTokens <= 1<<20 &&
		p.KeepRecentTokens > 0 && p.KeepRecentTokens <= 1<<30 &&
		p.MaxAttempts >= 1 && p.MaxAttempts <= 8
}

// These are local estimates, never provider-reported usage. Private replay
// data and image references remain in the accounting even when not displayed.
type ContextTokenEstimate struct {
	Method       string `json:"method"`
	System       int64  `json:"system"`
	Messages     int64  `json:"messages"`
	Tools        int64  `json:"tools"`
	Private      int64  `json:"private"`
	Images       int64  `json:"images"`
	Overhead     int64  `json:"overhead"`
	InputTokens  int64  `json:"input_tokens"`
	MessageCount int    `json:"message_count"`
	ImageCount   int    `json:"image_count"`
}

type ContextBudget struct {
	Policy               ContextBudgetPolicy  `json:"policy"`
	Estimate             ContextTokenEstimate `json:"estimate"`
	ContextWindow        int64                `json:"context_window"`
	OutputReserve        int64                `json:"output_reserve"`
	SummaryOutputReserve int64                `json:"summary_output_reserve"`
	// Nil limits mean unknown capacity, not an empty or unbounded window.
	InputLimit *int64 `json:"input_limit"`
	TriggerAt  *int64 `json:"trigger_at"`
	State      string `json:"state"`
}
