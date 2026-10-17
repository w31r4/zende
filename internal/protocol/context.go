package protocol

import "encoding/json"

// At pins a historical boundary, including zero for an empty initial context.
// RequestID selects the boundary saved before that exact model request.
type ContextTarget struct {
	AgentID   string `json:"agent_id"`
	At        *int64 `json:"at,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type ContextRead struct {
	ContextTarget
	Raw      bool  `json:"raw,omitempty"`
	After    int64 `json:"after,omitempty"`
	Limit    int   `json:"limit,omitempty"`
	MaxBytes int   `json:"max_bytes,omitempty"`
}

type ContextViewInfo struct {
	ID           string `json:"id"`
	Previous     string `json:"previous,omitempty"`
	SourceCursor int64  `json:"source_cursor"`
	Through      int64  `json:"through"`
	Published    int64  `json:"published"`
}

type ContextToolVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Request inspection is an allowlisted projection. Endpoint settings, provider
// binding bytes and resolved credentials never enter this public object.
type ContextRequestInfo struct {
	Configuration     *ConfigPublication   `json:"configuration,omitempty"`
	TaskConfiguration *ConfigPublication   `json:"task_configuration,omitempty"`
	ID                string               `json:"request_id"`
	TaskID            string               `json:"task_id"`
	Started           int64                `json:"started"`
	Model             string               `json:"model"`
	Protocol          string               `json:"protocol"`
	ToolSearch        string               `json:"tool_search,omitempty"`
	Selection         string               `json:"selection,omitempty"`
	Cwd               string               `json:"cwd,omitempty"`
	Effort            string               `json:"effort,omitempty"`
	SelectionRevision int64                `json:"selection_revision"`
	MaxOutputTokens   int                  `json:"max_output_tokens"`
	FormatRepair      bool                 `json:"format_repair"`
	Tools             []ContextToolVersion `json:"tools"`
	PromptSeq         int64                `json:"prompt_seq,omitempty"`
	ContextBudget     *ContextBudget       `json:"context_budget"`
}

type ContextInfo struct {
	AgentID     string              `json:"agent_id"`
	Cursor      int64               `json:"cursor"`
	View        *ContextViewInfo    `json:"view,omitempty"`
	Messages    int64               `json:"messages"`
	RawMessages int64               `json:"raw_messages"`
	Resources   *ResourceCatalog    `json:"resource_catalog"`
	Request     *ContextRequestInfo `json:"request,omitempty"`
}

// Content is a public ContextMessage or prompt instruction, not provider wire
// data. More means fetch that exact seq through context.entry; never truncate
// an individual JSON object to meet the page budget.
type ContextItem struct {
	Seq              int64           `json:"seq"`
	TaskID           string          `json:"task_id,omitempty"`
	Kind             string          `json:"kind"`
	Bytes            int             `json:"bytes"`
	SHA256           string          `json:"sha256"`
	PrivateDataBytes int             `json:"private_data_bytes,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	More             bool            `json:"more"`
}

type ContextPage struct {
	AgentID string           `json:"agent_id"`
	Cursor  int64            `json:"cursor"`
	Raw     bool             `json:"raw"`
	View    *ContextViewInfo `json:"view,omitempty"`
	Items   []ContextItem    `json:"items"`
	Next    int64            `json:"next"`
	More    bool             `json:"more"`
}

type ContextEntryRead struct {
	AgentID string `json:"agent_id"`
	Seq     int64  `json:"seq"`
	Offset  int    `json:"offset,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type ContextEntryPage struct {
	AgentID string      `json:"agent_id"`
	Entry   ContextItem `json:"entry"`
	Offset  int         `json:"offset"`
	Next    int         `json:"next"`
	Text    string      `json:"text"`
	More    bool        `json:"more"`
}
