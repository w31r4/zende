package protocol

import "encoding/json"

// MCPInvoke is model-facing. Agent, Task and operation identity come from the
// host dispatch, never from model-supplied arguments.
type MCPInvoke struct {
	ServerID  string          `json:"server_id"`
	ToolRef   string          `json:"tool_ref"`
	Arguments json.RawMessage `json:"arguments"`
}

type MCPCallTarget struct {
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
}

type MCPCallBinding struct {
	ResourceID      string          `json:"resource_id"`
	ResourceVersion string          `json:"resource_version"`
	Generation      int64           `json:"generation"`
	CatalogVersion  string          `json:"catalog_version"`
	ToolRef         string          `json:"tool_ref"`
	Definition      json.RawMessage `json:"definition"`
}

// Result previews are bounded; binary content is represented by retained part
// references. Reading a part never fetches a resource_link from its remote URI.
type MCPResultPart struct {
	Part      string `json:"part"`
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Text      string `json:"text,omitempty"`
	More      bool   `json:"more,omitempty"`
}
type MCPResultInfo struct {
	Bytes         int             `json:"bytes"`
	SHA256        string          `json:"sha256"`
	IsError       bool            `json:"is_error,omitempty"`
	ContentBlocks int             `json:"content_blocks"`
	Structured    bool            `json:"structured"`
	Parts         []MCPResultPart `json:"parts"`
	More          bool            `json:"more"`
}
type MCPCallReceipt struct {
	OperationID     string         `json:"operation_id"`
	AgentID         string         `json:"agent_id"`
	TaskID          string         `json:"task_id"`
	State           string         `json:"state"`
	ResourceID      string         `json:"resource_id"`
	ResourceVersion string         `json:"resource_version"`
	Generation      int64          `json:"generation"`
	CatalogVersion  string         `json:"catalog_version"`
	ToolRef         string         `json:"tool_ref"`
	Result          *MCPResultInfo `json:"result,omitempty"`
	Error           *Error         `json:"error,omitempty"`
}
type MCPResultRead struct {
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
	Part        string `json:"part,omitempty"` // empty/result, structured, or content index
	Offset      int    `json:"offset,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}
type MCPResultPage struct {
	MCPCallTarget
	Part      string `json:"part"`
	MediaType string `json:"media_type"`
	Encoding  string `json:"encoding"`
	Text      string `json:"text,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Offset    int    `json:"offset"`
	Next      int    `json:"next"`
	More      bool   `json:"more"`
}
