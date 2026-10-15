package protocol

// Path is resolved against the Agent cwd, never the client's cwd. Browsing is
// read-only. Selecting the returned Path is a separate durable operation.
type DirectoryQuery struct {
	AgentID string `json:"agent_id"`
	Path    string `json:"path,omitempty"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type DirectoryEntry struct {
	Name    string `json:"name"`
	Symlink bool   `json:"symlink,omitempty"`
}

type DirectoryPage struct {
	Path    string           `json:"path"`
	Parent  string           `json:"parent,omitempty"`
	Entries []DirectoryEntry `json:"entries"`
	Next    string           `json:"next,omitempty"`
	// Entries that could not be represented or statted during this scan.
	Skipped int `json:"skipped,omitempty"`
}

type GitQuery struct {
	AgentID string `json:"agent_id"`
	Path    string `json:"path,omitempty"`
}

// Counts are porcelain entries: an untracked directory counts once. Submodule
// internals are not scanned. Ahead/Behind use local refs, without a fetch.
type GitStatus struct {
	Path      string `json:"path"`
	State     string `json:"state"` // ready or none (not in a repository)
	Branch    string `json:"branch,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Detached  bool   `json:"detached,omitempty"`
	Unborn    bool   `json:"unborn,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Ahead     *int   `json:"ahead,omitempty"`
	Behind    *int   `json:"behind,omitempty"`
	Staged    int    `json:"staged"`
	Unstaged  int    `json:"unstaged"`
	Conflicts int    `json:"conflicts"`
	Untracked int    `json:"untracked"`
}
