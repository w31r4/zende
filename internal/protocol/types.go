// Package protocol defines the shared, UI-independent wire contracts.
package protocol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const Version = 1
const MaxFrameBytes = 8 << 20

// ID correlates one transport exchange. OperationID identifies a durable mutation
// across reconnects. A retry changes ID but must retain OperationID and params.
type Request struct {
	JSONRPC     string          `json:"jsonrpc"`
	Version     int             `json:"schema_version"`
	ID          string          `json:"id"`
	OperationID string          `json:"operation_id,omitempty"`
	Method      string          `json:"method"`
	Params      json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Version int             `json:"schema_version"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    ErrorData `json:"data"`
}

type ErrorData struct {
	Kind        string `json:"kind"`
	Retryable   bool   `json:"retryable"`
	OperationID string `json:"operation_id,omitempty"`
}

func (e *Error) Error() string { return e.Data.Kind + ": " + e.Message }

func E(kind, message string) *Error {
	code := -32000
	switch kind {
	case "invalid_request":
		code = -32600
	case "method_not_found":
		code = -32601
	case "invalid_params":
		code = -32602
	case "internal":
		code = -32603
	}
	return &Error{Code: code, Message: message, Data: ErrorData{Kind: kind}}
}

func NewID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func NewRequest(method, op string, params any) (Request, error) {
	p, err := json.Marshal(params)
	return Request{JSONRPC: "2.0", Version: Version, ID: NewID("rpc"), OperationID: op, Method: method, Params: p}, err
}

func (r Request) Validate() error {
	if r.JSONRPC != "2.0" || r.ID == "" || r.Method == "" || len(r.Params) == 0 || !json.Valid(r.Params) {
		return E("invalid_request", "jsonrpc, id, method and valid params are required")
	}
	if r.Version != Version {
		return E("version_mismatch", "unsupported schema_version")
	}
	if p := bytes.TrimSpace(r.Params); len(p) == 0 || p[0] != '{' {
		return E("invalid_params", "params must be an object")
	}
	return nil
}

// StrictDecode rejects unknown/duplicate fields and trailing documents. Values
// are deliberately omitted from errors, since malformed input can contain keys.
func StrictDecode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := checkValue(d); err != nil {
		return E("invalid_params", "invalid JSON or duplicate fields")
	}
	if _, err := d.Token(); err != io.EOF {
		return E("invalid_params", "expected a single JSON document")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return E("invalid_params", "unknown field or invalid field type")
	}
	return nil
}

func checkValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return fmt.Errorf("duplicate field")
			}
			seen[s] = true
			if err := checkValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

// Fingerprint excludes the transport ID and normalizes object key ordering.
func (r Request) Fingerprint() (string, error) {
	var p any
	d := json.NewDecoder(bytes.NewReader(r.Params))
	d.UseNumber()
	if err := d.Decode(&p); err != nil {
		return "", err
	}
	b, err := json.Marshal([]any{r.Version, r.Method, p})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type Event struct {
	Version int             `json:"schema_version"`
	AgentID string          `json:"agent_id"`
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	TaskID  string          `json:"task_id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  Event  `json:"params"`
}

type Agent struct {
	ID       string `json:"agent_id"`
	ParentID string `json:"parent_id,omitempty"`
	Cwd      string `json:"cwd"`
	State    string `json:"state"`
	Paused   bool   `json:"paused"`
	Model    string `json:"model,omitempty"`
	Seq      int64  `json:"seq"`
}

type Task struct {
	ID          string   `json:"task_id"`
	AgentID     string   `json:"agent_id"`
	State       string   `json:"state"`
	Input       string   `json:"input"`
	Result      string   `json:"result,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

type Snapshot struct {
	Agent     Agent          `json:"agent"`
	Tasks     []Task         `json:"tasks"`
	Cursor    int64          `json:"cursor"`
	Questions []QuestionInfo `json:"questions,omitempty"`
}

type Accepted struct {
	OperationID string        `json:"operation_id"`
	AgentID     string        `json:"agent_id"`
	TaskID      string        `json:"task_id,omitempty"`
	QuestionID  string        `json:"question_id,omitempty"`
	State       string        `json:"state"`
	Attachment  *Attachment   `json:"attachment,omitempty"`
	Resource    *ResourceInfo `json:"resource,omitempty"`
}

type CreateAgent struct {
	Cwd   string `json:"cwd"`
	Input string `json:"input,omitempty"`
	Model string `json:"model,omitempty"`
}

type Submit struct {
	AgentID     string   `json:"agent_id"`
	Input       string   `json:"input"`
	Attachments []string `json:"attachments,omitempty"`
}

type Target struct {
	AgentID string `json:"agent_id"`
}
type RecoveryTarget struct {
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id,omitempty"`
}
type Cursor struct {
	AgentID string `json:"agent_id"`
	After   int64  `json:"after"`
	Limit   int    `json:"limit,omitempty"`
}

type EventPage struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
	Latest int64   `json:"latest"`
}
