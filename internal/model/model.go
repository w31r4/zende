// Package model owns provider protocols and portable model messages, not UI state.
package model

import (
	"context"
	"encoding/json"
)

type Block struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Attachment string          `json:"attachment,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
}

type Message struct {
	Role     string          `json:"role"`
	Category string          `json:"category,omitempty"`
	Content  []Block         `json:"content"`
	Protocol string          `json:"protocol,omitempty"`
	Opaque   json.RawMessage `json:"opaque,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Request struct {
	ID              string    `json:"request_id"`
	Model           string    `json:"model"`
	System          string    `json:"system"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools"`
	MaxOutputTokens int       `json:"max_output_tokens"`
	// Resolved only for transport; durable context keeps attachment IDs.
	Images map[string]Image `json:"-"`
}

type Image struct {
	MediaType string
	Data      []byte
}

// Adapters emit semantic deltas, not provider wire objects. ResponseEnd means
// transport completion; only an explicitly categorized message can be final.
const (
	MessageStart    = "message.start"
	MessageCategory = "message.category"
	TextDelta       = "text.delta"
	MessageEnd      = "message.end"
	CallStart       = "call.start"
	ArgumentsDelta  = "arguments.delta"
	CallEnd         = "call.end"
	ProtocolData    = "protocol.data"
	UsageUpdate     = "usage"
	ResponseEnd     = "response.end"
)

type Usage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
}

type Delta struct {
	Kind      string          `json:"kind"`
	MessageID string          `json:"message_id,omitempty"`
	Category  string          `json:"category,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Text      string          `json:"text,omitempty"`
	Protocol  string          `json:"protocol,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
	Usage     *Usage          `json:"usage,omitempty"`
}

type Adapter interface {
	Stream(context.Context, Request, func(Delta) error) error
}

// A binding is fixed for one request. It never carries resolved credentials
// into request history; an adapter resolves those at its transport boundary.
type Binding struct {
	Adapter          Adapter
	Model, Protocol  string
	NativeCategories bool
	ToolSearch       string // Empty uses host search; native routes are explicitly configured.
	Images           bool
	MaxOutputTokens  int
	ContextWindow    int // Zero means unknown; never infer capacity from the model name.
	// Snapshot contains provider-owned versioned settings, never resolved keys.
	Snapshot json.RawMessage
}

type Resolver interface {
	Resolve(context.Context, string) (Binding, error)
}

type SelectionResolver interface {
	ResolveSelection(context.Context, string, string, string) (Binding, error)
}

// Optional transport preflight. It must not resolve credentials or send data.
type RequestValidator interface {
	ValidateRequest(Request) error
}

// ContextValidator checks a candidate model without sending a request. Unlike
// request validation, pending tool calls are permitted during selection.
type ContextValidator interface {
	ValidateContext(Request) error
}

// Restorer recreates the original transport from saved settings, independent
// of the current defaults/catalog. Unsupported or missing versions must fail.
type Restorer interface {
	Restore(context.Context, json.RawMessage) (Binding, error)
}

type ResolveFunc func(context.Context, string) (Binding, error)

func (f ResolveFunc) Resolve(ctx context.Context, name string) (Binding, error) { return f(ctx, name) }
