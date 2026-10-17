package protocol

type CompactContext struct {
	AgentID string  `json:"agent_id"`
	Caller  *Caller `json:"caller,omitempty"`
}

// CompactionInfo is the public projection. Provider bindings, transcript and
// protocol-private response data remain in the internal attempt journal.
type CompactionInfo struct {
	OperationID string      `json:"operation_id"`
	AgentID     string      `json:"agent_id"`
	Reason      string      `json:"reason"`
	State       string      `json:"state"`
	Attempts    int         `json:"attempts"`
	MaxAttempts int         `json:"max_attempts"`
	RequestID   string      `json:"request_id,omitempty"`
	ViewID      string      `json:"view_id,omitempty"`
	Source      *CallSource `json:"source,omitempty"`
	Failure     *Error      `json:"failure,omitempty"`
}
