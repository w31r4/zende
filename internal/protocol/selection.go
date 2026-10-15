package protocol

// Omitted fields retain the current choice. Effort is valid only with Model;
// an explicit empty effort means provider default, not the profile default.
type SelectAgent struct {
	AgentID          string  `json:"agent_id"`
	Cwd              *string `json:"cwd,omitempty"`
	Model            *string `json:"model,omitempty"`
	Effort           *string `json:"effort,omitempty"`
	CatalogVersion   string  `json:"catalog_version,omitempty"`
	ExpectedRevision *int64  `json:"expected_revision,omitempty"`
}

// Revision identifies the desired selection. model.started records what each
// request actually used; acceptance does not interrupt an existing request.
type AgentSelection struct {
	AgentID       string `json:"agent_id"`
	Cwd           string `json:"cwd"`
	Model         string `json:"model"`
	Effort        string `json:"effort"`
	Revision      int64  `json:"revision"`
	ModelRevision int64  `json:"model_revision"`
}
