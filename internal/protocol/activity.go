package protocol

// Activity is a bounded read-only observation, not a lifecycle owner. Processes
// are observed after the core transaction; Unavailable marks missing sources.
type Activity struct {
	AgentID     string         `json:"agent_id"`
	Items       []ActivityItem `json:"items"`
	Truncated   bool           `json:"truncated,omitempty"`
	Unavailable []string       `json:"unavailable,omitempty"`
}

type ActivityItem struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	AgentID string `json:"agent_id"`
	State   string `json:"state"`
}

const MaxActivityItems = 1000
