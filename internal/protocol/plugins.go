package protocol

// PluginCatalogQuery reads retained discovery data without starting a process.
// Empty Version selects the current publication; an exact version reads history.
type PluginCatalogQuery struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"resource_id"`
	Version string `json:"version,omitempty"`
}

// PluginCallTarget selects an original operation within one Agent. Reads never
// reconnect a plugin or grant permission to dispatch the operation again.
type PluginCallTarget struct {
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
}

type PluginProgressQuery struct {
	PluginCallTarget
	After int64 `json:"after,omitempty"`
	Limit int   `json:"limit,omitempty"`
}

type PluginResultRead struct {
	PluginCallTarget
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}
