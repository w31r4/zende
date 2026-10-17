package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

var toolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type savedBlock struct {
	Version  int             `json:"version"`
	Endpoint string          `json:"endpoint"`
	Model    string          `json:"model"`
	Block    json.RawMessage `json:"block"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	Citations json.RawMessage `json:"citations"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func decodeObject(raw []byte, value any) error {
	var fields map[string]json.RawMessage
	if protocol.StrictDecode(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, value) != nil {
		return wireError("invalid or duplicate JSON fields")
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	dx, dy := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	return json.Valid(a) && json.Valid(b) && dx.Decode(&x) == nil && dy.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// Runtime instructions stay at their original position. This is a host text
// convention, not an assertion that the API has a portable developer role.
const instructionGuide = "Runtime notices appear in user messages as <harness_instruction role=\"system\"> or <harness_instruction role=\"developer\"> blocks. Use them as task instructions from the harness."

func (a *Adapter) encode(request model.Request, allowPending bool) ([]byte, error) {
	if request.Model == "" || request.MaxOutputTokens < 1 {
		return nil, protocol.E("invalid_params", "Anthropic requires a model and positive output budget")
	}
	if a.config.Thinking == "enabled" && a.config.ThinkingBudget >= request.MaxOutputTokens {
		return nil, protocol.E("invalid_params", "Anthropic thinking budget must be smaller than max output")
	}
	tools, names := []any{}, map[string]bool{}
	for _, tool := range request.Tools {
		var schema map[string]any
		if !toolName.MatchString(tool.Name) || names[tool.Name] || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, wireError("invalid tool definition")
		}
		names[tool.Name] = true
		tools = append(tools, toolDefinition(tool, false))
	}
	searches := map[string]bool{}
	discovery := deferredTools{reserved: names, definitions: map[string]model.Tool{}}
	if a.config.ClientToolSearch && len(names) > maxSearchDefinitions {
		return nil, protocol.E("context_limit", "native search definitions exceed the request limit")
	}
	messages := []wireMessage{}
	appendParts := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, parts...)
		} else {
			messages = append(messages, wireMessage{Role: role, Content: parts})
		}
	}
	pending, seen := map[string]bool{}, map[string]bool{}
	imageBytes := 0
	lastRole := ""
	for _, message := range request.Messages {
		if message.Protocol != "" && message.Protocol != Protocol {
			return nil, protocol.E("protocol_mismatch", "history contains another protocol's private data; explicit conversion is required")
		}
		if message.Category != "" {
			return nil, protocol.E("unsupported_capability", "Anthropic history requires explicit text markers, not native categories")
		}
		if len(pending) != 0 && message.Role != "tool" && !(message.Role == "assistant" && lastRole == "assistant") && !allowPending {
			return nil, wireError("message interrupts pending tool results")
		}
		role := message.Role
		parts := []any{}
		if len(message.Opaque) != 0 {
			var saved savedBlock
			if role != "assistant" || message.Protocol != Protocol || protocol.StrictDecode(message.Opaque, &saved) != nil || (saved.Version != 1 && saved.Version != 2) || saved.Endpoint != a.endpoint || saved.Model != request.Model {
				return nil, protocol.E("protocol_mismatch", "Anthropic private block belongs to another endpoint/model or version")
			}
			var block wireBlock
			if err := decodeObject(saved.Block, &block); err != nil {
				return nil, err
			}
			if err := matchSavedBlock(block, message); err != nil {
				return nil, err
			}
			if saved.Version == 2 {
				if !a.config.ClientToolSearch || block.Type != "tool_use" || block.Name != searchToolName {
					return nil, protocol.E("protocol_mismatch", "saved native search requires the Anthropic deferred search route")
				}
				searches[block.ID] = true
			}
			if block.Type == "tool_use" {
				if seen[block.ID] {
					return nil, wireError("duplicate historical tool ID")
				}
				seen[block.ID], pending[block.ID] = true, true
			}
			parts = append(parts, saved.Block)
		} else {
			switch role {
			case "user", "system", "developer":
				for _, block := range message.Content {
					if block.Type == "image" && role == "user" {
						if !a.config.Images {
							return nil, protocol.E("unsupported_capability", "selected Anthropic model does not declare image input")
						}
						image, ok := request.Images[block.Attachment]
						if !ok || block.Attachment == "" {
							return nil, protocol.E("unsupported_capability", "Anthropic image input requires resolved local bytes")
						}
						if block.Text != "" || (image.MediaType != "image/png" && image.MediaType != "image/jpeg") || len(image.Data) == 0 || len(image.Data) > protocol.MaxImageBytes {
							return nil, protocol.E("invalid_params", "invalid resolved image input")
						}
						imageBytes += len(image.Data)
						if imageBytes > protocol.MaxRequestImageBytes {
							return nil, protocol.E("context_limit", "Anthropic images exceed the request budget")
						}
						parts = append(parts, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": image.MediaType, "data": base64.StdEncoding.EncodeToString(image.Data)}})
						continue
					}
					if block.Type != "text" || block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Anthropic input content is not supported")
					}
					text := block.Text
					if role != "user" {
						text = "<harness_instruction role=\"" + role + "\">\n" + text + "\n</harness_instruction>"
					}
					if text != "" {
						parts = append(parts, map[string]string{"type": "text", "text": text})
					}
				}
				role = "user"
			case "assistant":
				for _, block := range message.Content {
					if block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Anthropic assistant attachments are not supported")
					}
					switch block.Type {
					case "text":
						if block.Text != "" {
							parts = append(parts, map[string]string{"type": "text", "text": block.Text})
						}
					case "tool_call":
						var input map[string]json.RawMessage
						if !validCall(block.CallID, block.Name) || seen[block.CallID] || decodeObject(block.Arguments, &input) != nil {
							return nil, wireError("invalid historical tool call")
						}
						seen[block.CallID], pending[block.CallID] = true, true
						parts = append(parts, map[string]any{"type": "tool_use", "id": block.CallID, "name": block.Name, "input": block.Arguments})
					default:
						return nil, protocol.E("unsupported_capability", "Anthropic assistant content is not supported")
					}
				}
			case "tool":
				id, failed := "", false
				text := []string{}
				for _, block := range message.Content {
					if block.Type != "text" || block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Anthropic tool content is not supported")
					}
					if block.CallID == "" || id != "" && id != block.CallID {
						return nil, wireError("tool result mixes call IDs")
					}
					id, failed = block.CallID, failed || block.IsError
					text = append(text, block.Text)
				}
				if !pending[id] {
					return nil, wireError("orphan or repeated tool result")
				}
				delete(pending, id)
				var content any = strings.Join(text, "\n")
				if searches[id] {
					var err error
					content, err = discovery.result(strings.Join(text, "\n"), failed)
					if err != nil {
						return nil, err
					}
				}
				parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": failed})
				role = "user"
			default:
				return nil, wireError("unsupported history role")
			}
		}
		appendParts(role, parts)
		lastRole = message.Role
	}
	if len(pending) != 0 && !allowPending {
		return nil, wireError("historical calls are missing results")
	}
	if !allowPending && (len(messages) == 0 || messages[0].Role != "user" || messages[len(messages)-1].Role != "user") {
		return nil, wireError("Anthropic history must start and end with user input or tool results")
	}
	body := map[string]any{"model": request.Model, "messages": messages, "stream": true, "max_tokens": request.MaxOutputTokens, "system": strings.TrimSuffix(request.System, "\n") + "\n\n" + instructionGuide}
	for _, name := range discovery.order {
		tools = append(tools, toolDefinition(discovery.definitions[name], true))
	}
	if len(tools) != 0 {
		body["tools"] = tools
	}
	if a.config.ClientToolSearch && len(request.Tools) == 0 {
		// References in history still need their definitions. An empty active
		// tool set must not reactivate those functions during format repair.
		body["tool_choice"] = map[string]string{"type": "none"}
	}
	if a.config.Thinking != "" {
		thinking := map[string]any{"type": a.config.Thinking}
		if a.config.Thinking == "enabled" {
			thinking["budget_tokens"] = a.config.ThinkingBudget
		}
		if a.config.ThinkingDisplay != "" {
			thinking["display"] = a.config.ThinkingDisplay
		}
		body["thinking"] = thinking
	}
	if a.config.Effort != "" {
		body["output_config"] = map[string]string{"effort": a.config.Effort}
	}
	data, err := json.Marshal(body)
	if len(data) > 32<<20 {
		return nil, protocol.E("context_limit", "Anthropic request exceeds the transport budget")
	}
	return data, err
}

func validCall(id, name string) bool {
	return id != "" && len(id) <= 512 && toolName.MatchString(name)
}

func matchSavedBlock(block wireBlock, message model.Message) error {
	if block.Type == "tool_use" {
		var input map[string]json.RawMessage
		if len(message.Content) != 1 || !validCall(block.ID, block.Name) || decodeObject(block.Input, &input) != nil {
			return wireError("invalid saved tool call")
		}
		public := message.Content[0]
		if public.Type != "tool_call" || public.Attachment != "" || public.CallID != block.ID || public.Name != block.Name || !sameJSON(public.Arguments, block.Input) {
			return wireError("saved call conflicts with public content")
		}
		return nil
	}
	var text strings.Builder
	for _, public := range message.Content {
		if public.Type != "text" || public.Attachment != "" {
			return wireError("saved block conflicts with public content")
		}
		text.WriteString(public.Text)
	}
	switch block.Type {
	case "text":
		if text.String() != block.Text {
			return wireError("saved text conflicts with public content")
		}
	case "thinking":
		if text.Len() != 0 || block.Signature == "" {
			return wireError("thinking must retain a signature and cannot replace public text")
		}
	case "redacted_thinking":
		if text.Len() != 0 || block.Data == "" {
			return wireError("redacted thinking must retain its data and cannot replace public text")
		}
	default:
		return protocol.E("unsupported_capability", "Anthropic saved block type is not supported")
	}
	return nil
}
