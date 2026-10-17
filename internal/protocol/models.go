package protocol

type ConnectionQuery struct {
	AgentID string `json:"agent_id,omitempty"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Version string `json:"version,omitempty"`
}

type ModelQuery struct {
	AgentID    string `json:"agent_id,omitempty"`
	Connection string `json:"connection"`
	After      string `json:"after,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Version    string `json:"version,omitempty"`
}

// Supported describes the installed adapter, not network or credential health.
// Catalog reads never resolve credentials or query a provider's model endpoint.
type ConnectionInfo struct {
	ID        string `json:"connection"`
	Label     string `json:"label,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Protocol  string `json:"protocol"`
	Models    int    `json:"models"`
	Supported bool   `json:"supported"`
}

type ModelInfo struct {
	ID               string   `json:"model"`
	Label            string   `json:"label,omitempty"`
	ContextWindow    int      `json:"context_window"`
	MaxOutput        int      `json:"max_output"`
	Efforts          []string `json:"efforts"`
	Images           bool     `json:"images"`
	NativeCategories bool     `json:"native_categories"`
	ToolSearch       string   `json:"tool_search,omitempty"`
}

type ConnectionPage struct {
	Items        []ConnectionInfo `json:"items"`
	Next         string           `json:"next,omitempty"`
	Version      string           `json:"version"`
	DefaultModel string           `json:"default_model,omitempty"`
	Effort       string           `json:"default_effort,omitempty"`
}

type ModelPage struct {
	Connection ConnectionInfo `json:"connection"`
	Items      []ModelInfo    `json:"items"`
	Next       string         `json:"next,omitempty"`
	Version    string         `json:"version"`
}
