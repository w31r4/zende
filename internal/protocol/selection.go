package protocol

// Omitted Cwd/Model retain the current choice. Effort and ContextMode require
// Model; each model selection defaults to provider effort and strict history.
type SelectAgent struct {
	AgentID          string  `json:"agent_id"`
	Cwd              *string `json:"cwd,omitempty"`
	Model            *string `json:"model,omitempty"`
	Effort           *string `json:"effort,omitempty"`
	ContextMode      *string `json:"context_mode,omitempty"`
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
	ContextMode   string `json:"context_mode,omitempty"`
	Revision      int64  `json:"revision"`
	ModelRevision int64  `json:"model_revision"`
}

// These are local declarations, not a remote capability or credential probe.
// Zero context capacity remains unknown. No endpoint or private snapshot is
// exposed in a selection preview.
type SelectionModel struct {
	Selection        string `json:"selection"`
	Model            string `json:"model"`
	Protocol         string `json:"protocol"`
	Effort           string `json:"effort"`
	ContextWindow    int    `json:"context_window"`
	MaxOutput        int    `json:"max_output"`
	Images           bool   `json:"images"`
	NativeCategories bool   `json:"native_categories"`
	ToolSearch       string `json:"tool_search"`
	ContextMode      string `json:"context_mode"`
}

type SelectionPreview struct {
	AgentID      string             `json:"agent_id"`
	Revision     int64              `json:"revision"`
	Cursor       int64              `json:"cursor"`
	CurrentCwd   string             `json:"current_cwd"`
	Cwd          string             `json:"cwd"`
	Current      *SelectionModel    `json:"current"`
	CurrentError *Error             `json:"current_error,omitempty"`
	Candidate    SelectionModel     `json:"candidate"`
	Changes      []string           `json:"changes"`
	Compatible   bool               `json:"compatible"`
	Issues       []*Error           `json:"issues"`
	Projection   *ContextProjection `json:"context_projection,omitempty"`
}
