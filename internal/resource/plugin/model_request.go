package plugin

import (
	"encoding/json"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (a *ModelAdapter) ValidateRequest(r model.Request) error {
	return a.validate(r, false)
}

func (a *ModelAdapter) ValidateContext(r model.Request) error {
	return a.validate(r, true)
}

// Preflight is local. It neither resolves credentials nor asks the plugin to
// interpret history. Pending calls are permitted only during model selection.
func (a *ModelAdapter) validate(r model.Request, allowPending bool) error {
	bad := func() error { return protocol.E("invalid_params", "invalid public model request for plugin provider") }
	if !allowPending && !boundedID(r.ID, 128) || r.Model != a.info.ID || r.MaxOutputTokens < 1 || r.MaxOutputTokens > a.info.MaxOutput || !utf8.ValidString(r.System) {
		return bad()
	}
	seen, pending := map[string]bool{}, map[string]bool{}
	usedImages := map[string]bool{}
	for _, m := range r.Messages {
		if m.Protocol != "" && m.Protocol != a.protocol || len(m.Opaque) != 0 && (m.Protocol != a.protocol || m.Role != "assistant") {
			return protocol.E("protocol_mismatch", "private model data belongs to another plugin provider/version/model; explicit conversion is required")
		}
		if len(m.Opaque) > 0 {
			var value any
			if protocol.StrictDecode(m.Opaque, &value) != nil {
				return bad()
			}
		}
		if m.Category != "" && (m.Role != "assistant" || m.Category != "commentary" && m.Category != "final") {
			return bad()
		}
		switch m.Role {
		case "user", "system", "developer", "assistant", "tool":
		default:
			return bad()
		}
		resultID := ""
		for _, b := range m.Content {
			if !utf8.ValidString(b.Text) {
				return bad()
			}
			switch b.Type {
			case "text":
				if b.Attachment != "" || len(b.Arguments) != 0 || b.Name != "" {
					return bad()
				}
				if m.Role == "tool" {
					if !boundedID(b.CallID, 256) || resultID != "" && resultID != b.CallID {
						return bad()
					}
					resultID = b.CallID
				} else if b.CallID != "" || b.IsError {
					return bad()
				}
			case "tool_call":
				var args map[string]json.RawMessage
				if m.Role != "assistant" || !boundedID(b.CallID, 256) || !boundedID(b.Name, 256) || seen[b.CallID] || b.Attachment != "" || b.Text != "" || b.IsError || protocol.StrictDecode(b.Arguments, &args) != nil || args == nil {
					return bad()
				}
				seen[b.CallID], pending[b.CallID] = true, true
			case "image":
				if !a.info.Images {
					return protocol.E("unsupported_capability", "selected plugin model does not support images")
				}
				if m.Role != "user" || !boundedID(b.Attachment, 128) || b.Text != "" || b.CallID != "" || b.Name != "" || len(b.Arguments) > 0 || b.IsError {
					return bad()
				}
				usedImages[b.Attachment] = true
			default:
				return protocol.E("unsupported_capability", "plugin model request contains an unsupported block")
			}
		}
		if m.Role == "tool" {
			if resultID == "" || !pending[resultID] {
				return bad()
			}
			delete(pending, resultID)
		}
	}
	if len(pending) > 0 && !allowPending {
		return protocol.E("invalid_params", "historical tool calls are missing results")
	}
	if len(usedImages) != len(r.Images) {
		return protocol.E("invalid_params", "resolved images do not match model context references")
	}
	total := 0
	for id := range usedImages {
		img, ok := r.Images[id]
		if !ok || len(img.Data) == 0 || len(img.Data) > protocol.MaxImageBytes || img.MediaType != "image/png" && img.MediaType != "image/jpeg" {
			return protocol.E("invalid_params", "invalid or missing resolved image input")
		}
		total += len(img.Data)
	}
	if total > protocol.MaxRequestImageBytes {
		return protocol.E("context_limit", "plugin model images exceed the request byte budget")
	}
	names := map[string]bool{}
	for _, t := range r.Tools {
		var schema map[string]any
		if !boundedID(t.Name, 256) || names[t.Name] || !utf8.ValidString(t.Description) || protocol.StrictDecode(t.InputSchema, &schema) != nil || schema["type"] != "object" {
			return bad()
		}
		names[t.Name] = true
	}
	raw, err := json.Marshal(a.params(r))
	if err != nil {
		return bad()
	}
	// Reserve space for the JSON-RPC envelope added at dispatch.
	if len(raw) > MaxModelRequestBytes-1024 {
		return protocol.E("context_limit", "plugin model request exceeds the transport budget")
	}
	return nil
}
