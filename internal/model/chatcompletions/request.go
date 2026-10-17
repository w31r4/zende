package chatcompletions

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type privateMessage struct {
	Version        int    `json:"version"`
	Endpoint       string `json:"endpoint"`
	Model          string `json:"model"`
	TextHash       string `json:"text_sha256"`
	Content        string `json:"content"`
	Refusal        string `json:"refusal,omitempty"`
	ReasoningField string `json:"reasoning_field,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
}

func textHash(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }

// Reject duplicate JSON fields at any nesting level while allowing additive
// metadata on a wire object. Semantic extension fields are checked separately.
func decodeObject(raw []byte, value any) error {
	var fields map[string]json.RawMessage
	if protocol.StrictDecode(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, value) != nil {
		return wireError("invalid or duplicate JSON fields")
	}
	return nil
}

func (a *Adapter) encode(request model.Request, allowPending bool) ([]byte, error) {
	if _, err := model.NativeSearchImports(request, false); err != nil {
		return nil, err
	}
	if request.Model == "" || request.MaxOutputTokens < 1 {
		return nil, protocol.E("invalid_params", "Chat Completions requires a model and positive output budget")
	}
	messages := []map[string]any{}
	if request.System != "" {
		messages = append(messages, map[string]any{"role": a.config.InstructionRole, "content": request.System})
	}
	pending, seen := map[string]bool{}, map[string]bool{}
	imageBytes := 0
	var assistant map[string]any
	privateSeen := false
	flush := func() {
		if assistant != nil {
			if field := a.config.ReasoningField; field != "" {
				if _, ok := assistant[field]; !ok {
					assistant[field] = ""
				}
			}
			messages = append(messages, assistant)
			assistant = nil
			privateSeen = false
		}
	}
	for _, message := range request.Messages {
		if message.Protocol != "" && message.Protocol != Protocol {
			return nil, protocol.E("protocol_mismatch", "history contains another protocol's private data; explicit conversion is required")
		}
		if message.Category != "" {
			return nil, protocol.E("unsupported_capability", "Chat Completions history requires explicit text markers, not native categories")
		}
		if message.Role != "assistant" && len(message.Opaque) != 0 {
			return nil, wireError("private data must belong to an assistant message")
		}
		switch message.Role {
		case "user", "system", "developer":
			flush()
			if len(pending) != 0 && !allowPending {
				return nil, wireError("message interrupts pending tool results")
			}
			parts := []any{}
			var text strings.Builder
			hasImage := false
			for _, block := range message.Content {
				if block.Type == "image" && message.Role == "user" {
					if !a.config.Images {
						return nil, protocol.E("unsupported_capability", "selected Chat Completions model does not declare image input")
					}
					image, ok := request.Images[block.Attachment]
					if !ok || block.Attachment == "" {
						return nil, protocol.E("unsupported_capability", "Chat Completions image input requires resolved local bytes")
					}
					if block.Text != "" || (image.MediaType != "image/png" && image.MediaType != "image/jpeg") || len(image.Data) == 0 || len(image.Data) > protocol.MaxImageBytes {
						return nil, protocol.E("invalid_params", "invalid resolved image input")
					}
					imageBytes += len(image.Data)
					if imageBytes > protocol.MaxRequestImageBytes {
						return nil, protocol.E("context_limit", "Chat Completions images exceed the request budget")
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)}})
					hasImage = true
					continue
				}
				if block.Type != "text" || block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Chat Completions input content is not supported")
				}
				text.WriteString(block.Text)
				parts = append(parts, map[string]string{"type": "text", "text": block.Text})
			}
			var content any = text.String()
			if hasImage {
				content = parts
			}
			role := message.Role
			if role == "system" || role == "developer" {
				role = a.config.InstructionRole
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "assistant":
			if assistant == nil {
				if len(pending) != 0 && !allowPending {
					return nil, wireError("assistant interrupts pending tool results")
				}
				assistant = map[string]any{"role": "assistant", "content": ""}
			}
			var public strings.Builder
			for _, block := range message.Content {
				if block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Chat Completions assistant attachments are not supported")
				}
				switch block.Type {
				case "text":
					public.WriteString(block.Text)
				case "tool_call":
					var arguments map[string]json.RawMessage
					if block.CallID == "" || len(block.CallID) > 512 || seen[block.CallID] || !functionName.MatchString(block.Name) || decodeObject(block.Arguments, &arguments) != nil {
						return nil, wireError("invalid historical function call")
					}
					pending[block.CallID], seen[block.CallID] = true, true
					calls, _ := assistant["tool_calls"].([]any)
					assistant["tool_calls"] = append(calls, map[string]any{"id": block.CallID, "type": "function", "function": map[string]string{"name": block.Name, "arguments": string(block.Arguments)}})
				default:
					return nil, protocol.E("unsupported_capability", "Chat Completions assistant content is not supported")
				}
			}
			visible := public.String()
			if len(message.Opaque) != 0 {
				var saved privateMessage
				if message.Protocol != Protocol || protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Version != 1 || saved.Endpoint != a.endpoint || saved.Model != request.Model || saved.TextHash != textHash(visible) || saved.Content+saved.Refusal != visible || saved.ReasoningField != a.config.ReasoningField {
					return nil, protocol.E("protocol_mismatch", "Chat Completions private message belongs to another endpoint/model, settings or content")
				}
				for _, b := range message.Content {
					if b.Type != "text" {
						return nil, wireError("private text message contains a function call")
					}
				}
				if privateSeen {
					return nil, wireError("multiple private messages in one assistant turn")
				}
				if saved.Reasoning != "" && saved.ReasoningField == "" {
					return nil, wireError("private reasoning has no declared field")
				}
				visible = saved.Content
				privateSeen = true
				if saved.Refusal != "" {
					assistant["refusal"] = saved.Refusal
				}
				if saved.ReasoningField != "" {
					assistant[saved.ReasoningField] = saved.Reasoning
				}
			}
			assistant["content"] = assistant["content"].(string) + visible
		case "tool":
			flush()
			id := ""
			pieces := []string{}
			isError := false
			for _, block := range message.Content {
				if block.Type != "text" || block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Chat Completions tool content is not supported")
				}
				if block.CallID == "" || id != "" && id != block.CallID {
					return nil, wireError("tool result mixes call IDs")
				}
				id = block.CallID
				pieces = append(pieces, block.Text)
				isError = isError || block.IsError
			}
			if !pending[id] {
				return nil, wireError("orphan or repeated tool result")
			}
			delete(pending, id)
			text := strings.Join(pieces, "\n")
			if isError {
				text = "Tool error:\n" + text
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
		default:
			return nil, wireError("unsupported history role")
		}
	}
	flush()
	if len(pending) != 0 && !allowPending {
		return nil, wireError("historical calls are missing results")
	}
	body := map[string]any{"model": request.Model, "messages": messages, "stream": true, a.config.MaxTokensField: request.MaxOutputTokens}
	if a.config.Effort != "" {
		body["reasoning_effort"] = a.config.Effort
	}
	if a.config.IncludeUsage {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	tools := []any{}
	names := map[string]bool{}
	for _, tool := range request.Tools {
		var schema map[string]any
		if !functionName.MatchString(tool.Name) || names[tool.Name] || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, wireError("invalid function definition")
		}
		names[tool.Name] = true
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
	}
	if len(tools) != 0 {
		body["tools"] = tools
	}
	encoded, err := json.Marshal(body)
	if len(encoded) > 32<<20 {
		return nil, protocol.E("context_limit", "Chat Completions request exceeds the transport budget")
	}
	return encoded, err
}
