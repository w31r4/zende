package protocol

const (
	MaxResourceFileBytes = 1 << 20
	MaxResourceBytes     = 8 << 20
	MaxResourceFiles     = 256
	MaxInstructionBytes  = 128 << 10
	MaxInputSkills       = 8
)

// ResourceRef pins content; publication of a new version never retargets it.
type ResourceRef struct {
	ID      string `json:"resource_id"`
	Version string `json:"version"`
}

// IDs identify a source within a profile; versions identify its immutable bytes.
type ResourceInfo struct {
	ID                     string `json:"resource_id"`
	Kind                   string `json:"kind"`
	Name                   string `json:"name"`
	Description            string `json:"description"`
	Source                 string `json:"source"`
	Version                string `json:"version"`
	DisableModelInvocation bool   `json:"disable_model_invocation,omitempty"`
	Files                  int    `json:"files"`
	Bytes                  int64  `json:"bytes"`
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
type ResourceStatus struct {
	ResourceInfo
	State      string `json:"state"`
	Generation int64  `json:"generation"`
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
