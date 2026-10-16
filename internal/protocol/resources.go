package protocol

import (
	"encoding/hex"
	"strings"
)

const (
	MaxResourceFileBytes = 1 << 20
	MaxResourceBytes     = 8 << 20
	MaxResourceFiles     = 256
	MaxInstructionBytes  = 128 << 10
	MaxInputSkills       = 8
	MaxAgentResources    = 512
)

// ResourceRef pins content; publication of a new version never retargets it.
type ResourceRef struct {
	ID      string `json:"resource_id"`
	Version string `json:"version"`
}

// ResourceCatalog identifies the ready definitions visible at a request
// boundary. Membership does not mean that their instructions were applied.
type ResourceCatalog struct {
	Generation int64         `json:"generation"`
	Entries    []ResourceRef `json:"entries"`
}

func (c ResourceCatalog) Valid() bool {
	if c.Generation < 0 || len(c.Entries) > MaxAgentResources || (c.Generation == 0) != (len(c.Entries) == 0) {
		return false
	}
	previous := ""
	for _, ref := range c.Entries {
		id, err := hex.DecodeString(strings.TrimPrefix(ref.ID, "resource_"))
		version, versionErr := hex.DecodeString(ref.Version)
		if err != nil || len(id) != 16 || versionErr != nil || len(version) != 32 || ref.ID <= previous || ref.ID != "resource_"+hex.EncodeToString(id) || ref.Version != hex.EncodeToString(version) {
			return false
		}
		previous = ref.ID
	}
	return true
}

// IDs identify a source within a profile; versions identify its immutable bytes.
type ResourceInfo struct {
	ID                     string                `json:"resource_id"`
	Kind                   string                `json:"kind"`
	Name                   string                `json:"name"`
	Description            string                `json:"description"`
	Source                 string                `json:"source"`
	Version                string                `json:"version"`
	DisableModelInvocation bool                  `json:"disable_model_invocation,omitempty"`
	Files                  int                   `json:"files"`
	Bytes                  int64                 `json:"bytes"`
	PluginOrigin           *PluginResourceOrigin `json:"plugin_origin,omitempty"`
}

// PluginResourceOrigin identifies the plugin publication that supplied bytes.
// Reading this resource does not require that plugin process to remain alive.
type PluginResourceOrigin struct {
	Plugin        ResourceRef `json:"plugin"`
	DefinitionRef string      `json:"definition_ref"`
}

type RegisterResource struct {
	AgentID string `json:"agent_id"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
}
type ResourceTarget struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"resource_id"`
}

// MCPSearch searches one registered server. Resource listing discovers servers
// without expanding their tool catalogs into model context.
type MCPSearch struct {
	AgentID  string `json:"agent_id"`
	ID       string `json:"id"`
	Query    string `json:"query,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}
type ResourceRead struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"resource_id"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
}
type ResourceList struct {
	AgentID string `json:"agent_id"`
	Kind    string `json:"kind,omitempty"`
	Query   string `json:"query,omitempty"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type ResourcePage struct {
	Items      []ResourceInfo `json:"items"`
	Next       string         `json:"next,omitempty"`
	Generation int64          `json:"generation"`
}

// State describes the available publication. Update describes the latest
// preparation, which may have failed while the published version stays ready.
type ResourceStatus struct {
	ResourceInfo
	State      string          `json:"state"`
	Generation int64           `json:"generation"`
	Update     *ResourceUpdate `json:"update,omitempty"`
	Error      *Error          `json:"error,omitempty"`
}

// ResourceUpdate exposes preparation status independently of content versions.
type ResourceUpdate struct {
	AttemptID   string `json:"attempt_id"`
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Error       *Error `json:"error,omitempty"`
}
type ResourceFileInfo struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
}
type ResourceContent struct {
	ResourceInfo
	Path     string             `json:"path"`
	Text     string             `json:"text,omitempty"`
	Data     []byte             `json:"data,omitempty"`
	Encoding string             `json:"encoding"`
	Entries  []ResourceFileInfo `json:"entries,omitempty"`
}
