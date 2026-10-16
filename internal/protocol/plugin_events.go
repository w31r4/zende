package protocol

// PluginSubscribe selects an exact event definition from a published catalog.
// Direction belongs to that definition, not to a caller-provided override.
// The stable operation ID is also the subscription ID.
type PluginSubscribe struct {
	AgentID       string   `json:"agent_id"`
	ResourceID    string   `json:"resource_id"`
	DefinitionRef string   `json:"definition_ref"`
	After         int64    `json:"after,omitempty"`
	Kinds         []string `json:"kinds,omitempty"`
	Caller        *Caller  `json:"caller,omitempty"`
}

type PluginSubscriptionTarget struct {
	AgentID        string `json:"agent_id"`
	SubscriptionID string `json:"subscription_id"`
}

type PluginSubscriptionQuery struct {
	AgentID    string `json:"agent_id"`
	ResourceID string `json:"resource_id,omitempty"`
	After      string `json:"after,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type PluginEventsRead struct {
	PluginSubscriptionTarget
	After int64 `json:"after,omitempty"`
	Limit int   `json:"limit,omitempty"`
}
