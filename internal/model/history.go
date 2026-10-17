package model

import (
	"encoding/json"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

// PrivateHistoryMatcher checks only whether a private envelope belongs to the
// selected transport. It does not replace that adapter's content validation.
type PrivateHistoryMatcher interface {
	KeepsPrivateHistory(Message, string) bool
}

// PublicExport is produced by the originating protocol after validating its
// private/public correspondence. A nil Message represents private-only state,
// which stays in the source history and must never become visible text.
// NativeSearchCallID preserves a known search call's meaning for re-encoding;
// it is not inferred from an arbitrary function name or tool-result JSON.
type PublicExport struct {
	Message            *Message
	NativeSearchCallID string
}

func CloneMessage(message Message) Message {
	message.Opaque = append(json.RawMessage(nil), message.Opaque...)
	content := make([]Block, len(message.Content))
	for i, block := range message.Content {
		block.Arguments = append(json.RawMessage(nil), block.Arguments...)
		content[i] = block
	}
	message.Content = content
	return message
}

// PublicCopy is a detached copy, not a validator or an in-place history edit.
// Callers must validate private envelopes with their originating protocol first.
func PublicCopy(message Message) Message {
	message.Protocol, message.Opaque = "", nil
	return CloneMessage(message)
}

// ValidateAssistantExport rejects public fields that a source or target codec
// might otherwise ignore. Source-specific envelope validation is still needed.
func ValidateAssistantExport(message Message) error {
	bad := func() error {
		return protocol.E("context_invalid", "source assistant content cannot be exported without losing public fields")
	}
	if message.Role != "assistant" || message.Category != "" && message.Category != "commentary" && message.Category != "final" {
		return bad()
	}
	for _, block := range message.Content {
		if block.Attachment != "" || block.IsError || !utf8.ValidString(block.Text) {
			return bad()
		}
		switch block.Type {
		case "text":
			if block.CallID != "" || block.Name != "" || len(block.Arguments) != 0 {
				return bad()
			}
		case "tool_call":
			var arguments map[string]json.RawMessage
			if message.Category != "" || block.Text != "" || block.CallID == "" || len(block.CallID) > 512 || !utf8.ValidString(block.CallID) || block.Name == "" || len(block.Name) > 256 || !utf8.ValidString(block.Name) || protocol.StrictDecode(block.Arguments, &arguments) != nil || arguments == nil {
				return bad()
			}
		default:
			return bad()
		}
	}
	return nil
}

func PrivateOnlyExport(message Message) (PublicExport, error) {
	if message.Role != "assistant" || message.Category != "" {
		return PublicExport{}, protocol.E("context_invalid", "private-only history contains a public role or category")
	}
	for _, block := range message.Content {
		if block.Type != "text" || block.Text != "" || block.Attachment != "" || block.CallID != "" || block.Name != "" || len(block.Arguments) != 0 || block.IsError {
			return PublicExport{}, protocol.E("context_invalid", "private-only history contains public content")
		}
	}
	return PublicExport{}, nil
}
