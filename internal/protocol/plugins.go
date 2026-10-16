package protocol

// PluginCatalogQuery reads retained discovery data without starting a process.
// Empty Version selects the current publication; an exact version reads history.
type PluginCatalogQuery struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"resource_id"`
	Version string `json:"version,omitempty"`
}
