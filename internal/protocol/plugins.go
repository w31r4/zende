package protocol

import "encoding/json"

// PluginCommand names a published command, not a native model tool. Core assigns
// its working directory and Task from the caller's saved execution provenance.
type PluginCommand struct {
	AgentID       string          `json:"agent_id"`
	ResourceID    string          `json:"resource_id"`
	DefinitionRef string          `json:"definition_ref"`
	Arguments     json.RawMessage `json:"arguments"`
	Caller        *Caller         `json:"caller,omitempty"`
}

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
