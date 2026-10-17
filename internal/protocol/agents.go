package protocol

// AgentPage is a compact navigation view. Model bindings, context and task
// bodies remain behind the explicit selection/inspect/history interfaces.
type AgentPageQuery struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type AgentSummary struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	Cwd      string `json:"cwd"`
	State    string `json:"state"`
	Paused   bool   `json:"paused"`
}

type AgentPage struct {
	Items []AgentSummary `json:"items"`
	Next  string         `json:"next,omitempty"`
}
