package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const MaxResponseBytes = 4 << 20
const MaxBlockBytes = 1 << 20
const MaxResponseBlocks = 128

type ToolCall struct {
	ID        string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Response struct {
	Messages []Message       `json:"messages"`
	Calls    []ToolCall      `json:"calls"`
	Usage    *Usage          `json:"usage,omitempty"`
	Output   []PublicMessage `json:"output,omitempty"`
}

type responseBlock struct {
	id      string
	message Message
	call    bool
	closed  bool
	text    strings.Builder
}

// Collector assembles interleaved calls without executing anything. A caller
// must also check Stream's error; a partial response never authorizes tools.
type Collector struct {
	blocks   []*responseBlock
	messages map[string]*responseBlock
	calls    map[string]*responseBlock
	ended    bool
	bytes    int
	usage    *Usage
}

func (c *Collector) Add(d Delta) error {
	bad := func() error { return protocol.E("model_protocol", "invalid or incomplete model event sequence") }
	if c.ended {
		return bad()
	}
	c.bytes += len(d.Text) + len(d.Raw) + len(d.MessageID) + len(d.CallID) + len(d.Name)
	if c.bytes > MaxResponseBytes || len(d.Text)+len(d.Raw) > MaxBlockBytes {
		return protocol.E("output_limit", "model response exceeds the assembly budget")
	}
	if c.messages == nil {
		c.messages = map[string]*responseBlock{}
		c.calls = map[string]*responseBlock{}
	}
	switch d.Kind {
	case MessageStart, CallStart:
		if len(c.blocks) >= MaxResponseBlocks {
			return protocol.E("output_limit", "too many model response blocks")
		}
		b := &responseBlock{message: Message{Role: "assistant", Content: []Block{}}}
		if d.Kind == MessageStart {
			if d.MessageID == "" || c.messages[d.MessageID] != nil || d.Category != "" && d.Category != "commentary" && d.Category != "final" {
				return bad()
			}
			b.id, b.message.Category = d.MessageID, d.Category
			c.messages[d.MessageID] = b
		} else {
			if d.CallID == "" || d.Name == "" || c.calls[d.CallID] != nil {
				return bad()
			}
			b.id, b.call = d.CallID, true
			b.message.Content = []Block{{Type: "tool_call", CallID: d.CallID, Name: d.Name}}
			c.calls[d.CallID] = b
		}
		c.blocks = append(c.blocks, b)
	case MessageCategory:
		b := c.messages[d.MessageID]
		if b == nil || b.closed || (d.Category != "commentary" && d.Category != "final") || (b.message.Category != "" && b.message.Category != d.Category) {
			return bad()
		}
		b.message.Category = d.Category
	case TextDelta, ArgumentsDelta:
		b := c.messages[d.MessageID]
		if d.Kind == ArgumentsDelta {
			b = c.calls[d.CallID]
		}
		if b == nil || b.closed {
			return bad()
		}
		if b.text.Len()+len(d.Text)+len(b.message.Opaque) > MaxBlockBytes {
			return protocol.E("output_limit", "model block exceeds the assembly budget")
		}
		b.text.WriteString(d.Text)
	case ProtocolData:
		b := c.messages[d.MessageID]
		if d.CallID != "" {
			if d.MessageID != "" {
				return bad()
			}
			b = c.calls[d.CallID]
		}
		if b == nil || b.closed || b.message.Opaque != nil || d.Protocol == "" || !json.Valid(d.Raw) {
			return bad()
		}
		if b.text.Len()+len(d.Raw) > MaxBlockBytes {
			return protocol.E("output_limit", "model block exceeds the assembly budget")
		}
		b.message.Protocol, b.message.Opaque = d.Protocol, append(json.RawMessage(nil), d.Raw...)
	case MessageEnd, CallEnd:
		b := c.messages[d.MessageID]
		if d.Kind == CallEnd {
			b = c.calls[d.CallID]
		}
		if b == nil || b.closed || !utf8.ValidString(b.text.String()) {
			return bad()
		}
		if b.call {
			var arguments map[string]any
			if protocol.StrictDecode([]byte(b.text.String()), &arguments) != nil || arguments == nil {
				return bad()
			}
			b.message.Content[0].Arguments = json.RawMessage(b.text.String())
		} else {
			b.message.Content = append(b.message.Content, Block{Type: "text", Text: b.text.String()})
		}
		b.closed = true
	case UsageUpdate:
		if d.Usage == nil || d.Usage.InputTokens != nil && *d.Usage.InputTokens < 0 || d.Usage.OutputTokens != nil && *d.Usage.OutputTokens < 0 {
			return bad()
		}
		data, _ := json.Marshal(d.Usage)
		if err := json.Unmarshal(data, &c.usage); err != nil {
			return err
		}
	case ResponseEnd:
		for _, b := range c.blocks {
			if !b.closed {
				return bad()
			}
		}
		c.ended = true
	default:
		return bad()
	}
	return nil
}

func (c *Collector) Response() (Response, error) {
	if !c.ended {
		return Response{}, protocol.E("model_incomplete", "model stream ended without a complete response")
	}
	r := Response{Messages: []Message{}, Calls: []ToolCall{}, Usage: c.usage}
	for _, b := range c.blocks {
		r.Messages = append(r.Messages, b.message)
		if b.call {
			call := b.message.Content[0]
			r.Calls = append(r.Calls, ToolCall{ID: call.CallID, Name: call.Name, Arguments: call.Arguments})
		}
	}
	// Copy owned slices before exposing a completed response to another layer.
	data, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	err = json.Unmarshal(data, &r)
	return r, err
}

// Final checks explicit categories only. Text-marker parsing and bounded repair
// are a separate adapter step; an uncategorized response cannot finish a task.
func (r Response) Final() (string, bool, error) {
	var final string
	found := false
	output := r.Output
	if output == nil {
		// Native-only callers can use Final without a streaming classifier.
		for _, m := range r.Messages {
			if m.Category == "" {
				continue
			}
			text := ""
			for _, b := range m.Content {
				if b.Type == "text" {
					text += b.Text
				}
			}
			output = append(output, PublicMessage{Category: m.Category, Text: text})
		}
	}
	for _, m := range output {
		if found {
			return "", false, protocol.E("output_format", "content follows the final message")
		}
		if m.Category == "final" {
			found = true
			final = m.Text
		}
	}
	if found && len(r.Calls) != 0 {
		return "", false, protocol.E("output_format", "final conflicts with tool calls")
	}
	return final, found, nil
}

func (r Response) ValidateProtocol(expected string) error {
	for _, m := range r.Messages {
		if m.Protocol != "" && m.Protocol != expected {
			return fmt.Errorf("model protocol data does not match selected adapter")
		}
	}
	return nil
}
