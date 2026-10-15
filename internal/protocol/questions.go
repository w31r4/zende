package protocol

const MaxQuestionBytes = 8 << 10
const MaxAnswerBytes = 1 << 20
const MaxOpenQuestions = 8

// QuestionInfo is the bounded projection used by lists and snapshots. Answers
// are available through question.inspect and the append-only event history.
type QuestionInfo struct {
	ID           string `json:"question_id"`
	AgentID      string `json:"agent_id"`
	TaskID       string `json:"task_id"`
	Prompt       string `json:"prompt"`
	State        string `json:"state"`
	DeliveredSeq int64  `json:"delivered_seq,omitempty"`
}

type Question struct {
	QuestionInfo
	Answer      string        `json:"answer,omitempty"`
	Attachments []string      `json:"attachments,omitempty"`
	Skills      []ResourceRef `json:"skills,omitempty"`
}

type AskQuestion struct {
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id"`
	Prompt  string `json:"prompt"`
}

type QuestionTarget struct {
	AgentID string `json:"agent_id"`
	ID      string `json:"question_id"`
}

type AnswerQuestion struct {
	AgentID     string        `json:"agent_id"`
	ID          string        `json:"question_id"`
	Answer      string        `json:"answer"`
	Attachments []string      `json:"attachments,omitempty"`
	Skills      []ResourceRef `json:"skills,omitempty"`
}

type QuestionList struct {
	AgentID string `json:"agent_id"`
	State   string `json:"state,omitempty"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type QuestionPage struct {
	Items []QuestionInfo `json:"items"`
	Next  string         `json:"next,omitempty"`
}
