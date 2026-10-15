package protocol

const MaxMailboxBytes = 256 << 10
const MaxPendingMail = 128

type SendMessage struct {
	AgentID  string  `json:"agent_id"`
	Input    string  `json:"input"`
	Delivery string  `json:"delivery"`
	Caller   *Caller `json:"caller,omitempty"`
}

// MailInfo separates durable delivery from the receiving task's outcome.
type MailInfo struct {
	ID           string         `json:"message_id"`
	AgentID      string         `json:"agent_id"`
	OperationID  string         `json:"operation_id"`
	Source       *CallSource    `json:"source,omitempty"`
	Result       *TaskResultRef `json:"result,omitempty"`
	SenderKind   string         `json:"sender_kind"`
	Delivery     string         `json:"delivery"`
	Seq          int64          `json:"accepted_seq"`
	SentSeq      int64          `json:"sent_seq,omitempty"`
	Bytes        int            `json:"bytes"`
	TaskID       string         `json:"task_id,omitempty"`
	DeliveredSeq int64          `json:"delivered_seq,omitempty"`
	State        string         `json:"state"`
	TaskState    string         `json:"task_state,omitempty"`
}
type Mail struct {
	MailInfo
	Input string `json:"input"`
}
type MailboxQuery struct {
	AgentID   string `json:"agent_id"`
	Direction string `json:"direction,omitempty"`
	State     string `json:"state,omitempty"`
	After     int64  `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type MailboxPage struct {
	Items  []MailInfo `json:"items"`
	Next   int64      `json:"next"`
	Latest int64      `json:"latest"`
	More   bool       `json:"more"`
}
type MailTarget struct {
	ID string `json:"message_id"`
}
