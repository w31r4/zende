package protocol

// Confirmation precedes any freeze or stop. Drafts are retained input, never
// instructions to execute during copying.
type CopyAgent struct {
	AgentID          string      `json:"agent_id"`
	Caller           *Caller     `json:"caller,omitempty"`
	Cwd              string      `json:"cwd"`
	Confirm          bool        `json:"confirm"`
	ExpectedRevision *int64      `json:"expected_revision,omitempty"`
	Drafts           []CopyDraft `json:"drafts,omitempty"`
}

const MaxCopyDrafts = 17 // Main composer plus 16 retained answer composers.
const MaxCopyDraftBytes = 6 << 20

type CopyDraft struct {
	ID            string        `json:"draft_id"`
	QuestionID    string        `json:"source_question_id,omitempty"`
	QuestionAgent string        `json:"source_question_agent_id,omitempty"`
	Text          string        `json:"text"`
	Attachments   []string      `json:"attachments,omitempty"`
	Skills        []ResourceRef `json:"skills,omitempty"`
}

// Drafts, when supplied, remaps a client's newer source drafts without changing
// the retained copy intent. Omitting it reads the originally accepted drafts.
type CopyDraftQuery struct {
	ID     string      `json:"operation_id"`
	Drafts []CopyDraft `json:"drafts,omitempty"`
}

// Public progress excludes private provider bindings and draft bodies.
type SessionCopyInfo struct {
	OperationID  string `json:"operation_id"`
	SourceAgent  string `json:"source_agent_id"`
	AgentID      string `json:"agent_id"`
	Cwd          string `json:"cwd"`
	State        string `json:"state"`
	Stage        string `json:"stage"`
	SourceCursor int64  `json:"source_cursor,omitempty"`
	Cursor       int64  `json:"cursor,omitempty"`
	Drafts       int    `json:"drafts"`
	Failure      *Error `json:"failure,omitempty"`
}

type CopiedEventSource struct {
	AgentID string `json:"agent_id"`
	Seq     int64  `json:"seq"`
}

type SessionCopyDrafts struct {
	Copy   SessionCopyInfo `json:"copy"`
	Drafts []CopyDraft     `json:"drafts"`
}
