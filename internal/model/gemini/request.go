package gemini

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

var functionName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Parts retain their original order and signature association. Public text can
// be assembled across transport chunks without merging the saved wire parts.
type savedParts struct {
	Version  int               `json:"version"`
	Endpoint string            `json:"endpoint"`
	Model    string            `json:"model"`
	CallID   string            `json:"call_id,omitempty"`
	Parts    []json.RawMessage `json:"parts"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type part struct {
	Text      *string         `json:"text,omitempty"`
	Thought   bool            `json:"thought,omitempty"`
	Signature string          `json:"thoughtSignature,omitempty"`
	Call      *functionCall   `json:"functionCall,omitempty"`
	Metadata  json.RawMessage `json:"partMetadata,omitempty"`
}

type wireContent struct {
	Role  string `json:"role"`
	Parts []any  `json:"parts"`
}

type callBinding struct{ name, wireID string }

func decodeObject(raw []byte, value any) error {
	var fields map[string]json.RawMessage
	if protocol.StrictDecode(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, value) != nil {
		return wireError("invalid or duplicate JSON fields")
	}
	return nil
}

func decodePart(raw []byte) (part, error) {
	var value part
	if protocol.StrictDecode(raw, &value) != nil {
		return value, protocol.E("unsupported_capability", "Gemini part is malformed or contains unsupported content")
	}
	if value.Call != nil {
		if value.Text != nil || value.Thought || !functionName.MatchString(value.Call.Name) || len(value.Call.ID) > 512 {
			return value, wireError("invalid function call part")
		}
		if _, err := arguments(value.Call.Args); err != nil {
			return value, err
		}
	} else if value.Text == nil && value.Signature == "" {
		return value, wireError("part has no supported content")
	}
	return value, nil
}

func arguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var object map[string]json.RawMessage
	if decodeObject(raw, &object) != nil {
		return nil, wireError("function arguments must be a JSON object")
	}
	return raw, nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	dx, dy := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	return json.Valid(a) && json.Valid(b) && dx.Decode(&x) == nil && dy.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

const instructionGuide = "Runtime notices appear in user messages as <harness_instruction role=\"system\"> or <harness_instruction role=\"developer\"> blocks. Use them as task instructions from the harness."

func (a *Adapter) encode(request model.Request, allowPending bool) ([]byte, error) {
	if !modelName.MatchString(strings.TrimPrefix(request.Model, "models/")) || request.MaxOutputTokens < 1 {
		return nil, protocol.E("invalid_params", "Gemini requires a valid model name and positive output budget")
	}
	if budget := a.config.ThinkingBudget; budget != nil && *budget >= request.MaxOutputTokens {
		return nil, protocol.E("invalid_params", "Gemini thinking budget must be smaller than max output")
	}
	contents := []wireContent{}
	appendParts := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if len(contents) > 0 && contents[len(contents)-1].Role == role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
		} else {
			contents = append(contents, wireContent{Role: role, Parts: parts})
		}
	}
	pending, seen := map[string]callBinding{}, map[string]bool{}
	lastRole, imageBytes := "", 0
	for _, message := range request.Messages {
		if message.Protocol != "" && message.Protocol != Protocol {
			return nil, protocol.E("protocol_mismatch", "history contains another protocol's private data; explicit conversion is required")
		}
		if message.Category != "" {
			return nil, protocol.E("unsupported_capability", "Gemini history requires explicit text markers, not native categories")
		}
		if len(pending) != 0 && message.Role != "tool" && !(message.Role == "assistant" && lastRole == "assistant") && !allowPending {
			return nil, wireError("message interrupts pending function results")
		}
		role, parts := message.Role, []any{}
		if len(message.Opaque) != 0 {
			var saved savedParts
			if role != "assistant" || message.Protocol != Protocol || protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Version != 1 || saved.Endpoint != a.endpoint || saved.Model != request.Model || len(saved.Parts) == 0 {
				return nil, protocol.E("protocol_mismatch", "Gemini private parts belong to another endpoint/model or version")
			}
			binding, err := matchSavedParts(saved, message)
			if err != nil {
				return nil, err
			}
			if saved.CallID != "" {
				if seen[saved.CallID] {
					return nil, wireError("duplicate historical function call ID")
				}
				seen[saved.CallID], pending[saved.CallID] = true, binding
			}
			for _, raw := range saved.Parts {
				parts = append(parts, raw)
			}
			role = "model"
		} else {
			switch role {
			case "user", "system", "developer":
				for _, block := range message.Content {
					if block.Type == "image" && role == "user" {
						if !a.config.Images {
							return nil, protocol.E("unsupported_capability", "selected Gemini model does not declare image input")
						}
						image, ok := request.Images[block.Attachment]
						if !ok || block.Attachment == "" {
							return nil, protocol.E("unsupported_capability", "Gemini image input requires resolved local bytes")
						}
						if block.Text != "" || (image.MediaType != "image/png" && image.MediaType != "image/jpeg") || len(image.Data) == 0 || len(image.Data) > protocol.MaxImageBytes {
							return nil, protocol.E("invalid_params", "invalid resolved image input")
						}
						imageBytes += len(image.Data)
						if imageBytes > protocol.MaxRequestImageBytes {
							return nil, protocol.E("context_limit", "Gemini images exceed the request budget")
						}
						parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": image.MediaType, "data": base64.StdEncoding.EncodeToString(image.Data)}})
						continue
					}
					if block.Type != "text" || block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Gemini input content is not supported")
					}
					text := block.Text
					if role != "user" {
						text = "<harness_instruction role=\"" + role + "\">\n" + text + "\n</harness_instruction>"
					}
					if text != "" {
						parts = append(parts, map[string]string{"text": text})
					}
				}
				role = "user"
			case "assistant":
				for _, block := range message.Content {
					if block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Gemini assistant attachments are not supported")
					}
					switch block.Type {
					case "text":
						if block.Text != "" {
							parts = append(parts, map[string]string{"text": block.Text})
						}
					case "tool_call":
						args, err := arguments(block.Arguments)
						if err != nil || len(block.Arguments) == 0 || block.CallID == "" || len(block.CallID) > 512 || seen[block.CallID] || !functionName.MatchString(block.Name) {
							return nil, wireError("invalid historical function call")
						}
						seen[block.CallID], pending[block.CallID] = true, callBinding{name: block.Name}
						parts = append(parts, map[string]any{"functionCall": map[string]any{"name": block.Name, "args": args}})
					default:
						return nil, protocol.E("unsupported_capability", "Gemini assistant content is not supported")
					}
				}
				role = "model"
			case "tool":
				id, failed, text := "", false, []string{}
				for _, block := range message.Content {
					if block.Type != "text" || block.Attachment != "" {
						return nil, protocol.E("unsupported_capability", "Gemini tool content is not supported")
					}
					if block.CallID == "" || id != "" && id != block.CallID {
						return nil, wireError("function result mixes call IDs")
					}
					id, failed = block.CallID, failed || block.IsError
					text = append(text, block.Text)
				}
				binding, ok := pending[id]
				if !ok {
					return nil, wireError("orphan or repeated function result")
				}
				delete(pending, id)
				key := "output"
				if failed {
					key = "error"
				}
				response := map[string]any{"name": binding.name, "response": map[string]string{key: strings.Join(text, "\n")}}
				if binding.wireID != "" {
					response["id"] = binding.wireID
				}
				parts = append(parts, map[string]any{"functionResponse": response})
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
	if !allowPending && (len(contents) == 0 || contents[0].Role != "user" || contents[len(contents)-1].Role != "user") {
		return nil, wireError("Gemini history must start and end with user input or function results")
	}
	generation := map[string]any{"maxOutputTokens": request.MaxOutputTokens}
	if a.config.Effort != "" || a.config.ThinkingBudget != nil || a.config.IncludeThoughts {
		thinking := map[string]any{}
		if a.config.Effort != "" {
			thinking["thinkingLevel"] = a.config.Effort
		}
		if a.config.ThinkingBudget != nil {
			thinking["thinkingBudget"] = *a.config.ThinkingBudget
		}
		if a.config.IncludeThoughts {
			thinking["includeThoughts"] = true
		}
		generation["thinkingConfig"] = thinking
	}
	body := map[string]any{"contents": contents, "generationConfig": generation, "systemInstruction": map[string]any{"parts": []any{map[string]string{"text": strings.TrimSuffix(request.System, "\n") + "\n\n" + instructionGuide}}}}
	declarations, names := []any{}, map[string]bool{}
	for _, tool := range request.Tools {
		var schema map[string]any
		if !functionName.MatchString(tool.Name) || names[tool.Name] || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, wireError("invalid function definition")
		}
		names[tool.Name] = true
		declarations = append(declarations, map[string]any{"name": tool.Name, "description": tool.Description, "parametersJsonSchema": tool.InputSchema})
	}
	if len(declarations) != 0 {
		body["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	data, err := json.Marshal(body)
	if len(data) > 32<<20 {
		return nil, protocol.E("context_limit", "Gemini request exceeds the transport budget")
	}
	return data, err
}

func matchSavedParts(saved savedParts, message model.Message) (callBinding, error) {
	var visible strings.Builder
	var call *functionCall
	for _, raw := range saved.Parts {
		p, err := decodePart(raw)
		if err != nil {
			return callBinding{}, err
		}
		if p.Call != nil {
			if call != nil || len(saved.Parts) != 1 {
				return callBinding{}, wireError("saved call mixes unrelated parts")
			}
			call = p.Call
		} else if p.Text != nil && !p.Thought {
			visible.WriteString(*p.Text)
		}
	}
	if call != nil {
		if saved.CallID == "" || len(saved.CallID) > 512 || len(message.Content) != 1 {
			return callBinding{}, wireError("saved call identity is missing")
		}
		args, err := arguments(call.Args)
		b := message.Content[0]
		if err != nil || b.Type != "tool_call" || b.Attachment != "" || b.CallID != saved.CallID || b.Name != call.Name || !sameJSON(b.Arguments, args) {
			return callBinding{}, wireError("saved call conflicts with public content")
		}
		return callBinding{name: call.Name, wireID: call.ID}, nil
	}
	if saved.CallID != "" {
		return callBinding{}, wireError("saved text has a call identity")
	}
	var text strings.Builder
	for _, b := range message.Content {
		if b.Type != "text" || b.Attachment != "" {
			return callBinding{}, wireError("saved parts conflict with public content")
		}
		text.WriteString(b.Text)
	}
	if text.String() != visible.String() {
		return callBinding{}, wireError("saved parts conflict with public text")
	}
	return callBinding{}, nil
}
