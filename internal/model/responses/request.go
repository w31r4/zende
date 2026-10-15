package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const Protocol = "responses"

type savedItem struct {
	Version    int             `json:"version"`
	Endpoint   string          `json:"endpoint"`
	Model      string          `json:"model"`
	ResponseID string          `json:"response_id,omitempty"`
	Item       json.RawMessage `json:"item"`
}

type wirePart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type wireItem struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Role      string     `json:"role"`
	Phase     string     `json:"phase"`
	Status    string     `json:"status"`
	CallID    string     `json:"call_id"`
	Name      string     `json:"name"`
	Arguments string     `json:"arguments"`
	Content   []wirePart `json:"content"`
}

func category(phase string) (string, error) {
	switch phase {
	case "":
		return "", nil
	case "commentary":
		return "commentary", nil
	case "final_answer":
		return "final", nil
	default:
		return "", wireError("unsupported assistant phase")
	}
}

func itemText(item wireItem) (string, error) {
	var text strings.Builder
	for _, part := range item.Content {
		switch part.Type {
		case "output_text":
			text.WriteString(part.Text)
		case "refusal":
			text.WriteString(part.Refusal)
		default:
			return "", protocol.E("unsupported_capability", "Responses output content type is not supported")
		}
	}
	return text.String(), nil
}

func decodeObject(data []byte, value any) error {
	var object map[string]json.RawMessage
	if protocol.StrictDecode(data, &object) != nil || object == nil || json.Unmarshal(data, value) != nil {
		return wireError("invalid or duplicate JSON fields")
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	// Persistence may compact JSON and escape <, > or & in RawMessage. Compare
	// values without losing integer precision or changing strings inside arguments.
	if !json.Valid(a) || !json.Valid(b) {
		return false
	}
	var x, y any
	dx, dy := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	return dx.Decode(&x) == nil && dy.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

func buildRequest(request model.Request, endpoint, effort string) ([]byte, error) {
	return encodeRequest(request, endpoint, effort, false)
}

func encodeRequest(request model.Request, endpoint, effort string, allowPending bool) ([]byte, error) {
	if request.Model == "" || request.MaxOutputTokens < 1 {
		return nil, protocol.E("invalid_params", "Responses requires a model and positive output budget")
	}
	input := []any{}
	imageBytes := 0
	pending := map[string]bool{}
	seen := map[string]bool{}
	appendCall := func(id string) error {
		if id == "" || seen[id] {
			return wireError("missing or duplicate historical call ID")
		}
		pending[id], seen[id] = true, true
		return nil
	}
	for _, message := range request.Messages {
		if message.Protocol != "" && message.Protocol != Protocol {
			return nil, protocol.E("protocol_mismatch", "history contains another protocol's private data; explicit conversion is required")
		}
		if len(message.Opaque) != 0 {
			var saved savedItem
			if message.Role != "assistant" || message.Protocol != Protocol || protocol.StrictDecode(message.Opaque, &saved) != nil || saved.Version != 1 || saved.Endpoint != endpoint || saved.Model != request.Model {
				return nil, protocol.E("protocol_mismatch", "Responses private item belongs to another endpoint/model or version")
			}
			var item wireItem
			if err := decodeObject(saved.Item, &item); err != nil {
				return nil, err
			}
			switch item.Type {
			case "message":
				text, err := itemText(item)
				if err != nil {
					return nil, err
				}
				var actual strings.Builder
				for _, block := range message.Content {
					if block.Type != "text" || block.Attachment != "" {
						return nil, wireError("saved message conflicts with public content")
					}
					actual.WriteString(block.Text)
				}
				if item.Role != "assistant" || actual.String() != text {
					return nil, wireError("saved message conflicts with public content")
				}
				cat, err := category(item.Phase)
				if err != nil || cat != message.Category {
					return nil, wireError("saved message conflicts with public category")
				}
			case "function_call":
				if len(message.Content) != 1 {
					return nil, wireError("saved call conflicts with public content")
				}
				block := message.Content[0]
				if block.Type != "tool_call" || block.CallID != item.CallID || block.Name != item.Name || !sameJSON(block.Arguments, []byte(item.Arguments)) {
					return nil, wireError("saved call conflicts with public content")
				}
				if err := appendCall(item.CallID); err != nil {
					return nil, err
				}
			case "reasoning":
				for _, block := range message.Content {
					if block.Type != "text" || block.Text != "" || block.Attachment != "" {
						return nil, wireError("reasoning item must not replace public content")
					}
				}
			default:
				return nil, protocol.E("unsupported_capability", "Responses saved item type is not supported")
			}
			input = append(input, saved.Item)
			continue
		}
		switch message.Role {
		case "user", "system", "developer":
			content := []map[string]string{}
			for _, block := range message.Content {
				if block.Type == "image" && message.Role == "user" {
					image, ok := request.Images[block.Attachment]
					if !ok || block.Attachment == "" {
						return nil, protocol.E("unsupported_capability", "Responses image input requires resolved local bytes")
					}
					if block.Text != "" || (image.MediaType != "image/png" && image.MediaType != "image/jpeg") || len(image.Data) == 0 || len(image.Data) > protocol.MaxImageBytes {
						return nil, protocol.E("invalid_params", "invalid resolved image input")
					}
					imageBytes += len(image.Data)
					if imageBytes > protocol.MaxRequestImageBytes {
						return nil, protocol.E("context_limit", "Responses images exceed the request byte budget")
					}
					content = append(content, map[string]string{"type": "input_image", "image_url": "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data), "detail": "auto"})
					continue
				}
				if block.Type != "text" || block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Responses attachment input is not enabled")
				}
				content = append(content, map[string]string{"type": "input_text", "text": block.Text})
			}
			input = append(input, map[string]any{"role": message.Role, "content": content})
		case "assistant":
			for _, block := range message.Content {
				if block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Responses assistant attachment is not enabled")
				}
				switch block.Type {
				case "text":
					item := map[string]any{"role": "assistant", "content": block.Text}
					if message.Category == "commentary" {
						item["phase"] = "commentary"
					} else if message.Category == "final" {
						item["phase"] = "final_answer"
					}
					input = append(input, item)
				case "tool_call":
					var args map[string]any
					if decodeObject(block.Arguments, &args) != nil || block.Name == "" {
						return nil, wireError("invalid historical tool arguments")
					}
					if err := appendCall(block.CallID); err != nil {
						return nil, err
					}
					input = append(input, map[string]any{"type": "function_call", "call_id": block.CallID, "name": block.Name, "arguments": string(block.Arguments)})
				default:
					return nil, protocol.E("unsupported_capability", "Responses assistant content is not supported")
				}
			}
		case "tool":
			id := ""
			var pieces []string
			isError := false
			for _, block := range message.Content {
				if block.Type != "text" || block.Attachment != "" {
					return nil, protocol.E("unsupported_capability", "Responses tool attachment is not enabled")
				}
				if block.CallID == "" || id != "" && block.CallID != id {
					return nil, wireError("tool result mixes call IDs")
				}
				id = block.CallID
				pieces = append(pieces, block.Text)
				isError = isError || block.IsError
			}
			if id == "" || !pending[id] {
				return nil, wireError("orphan or repeated tool result")
			}
			delete(pending, id)
			text := strings.Join(pieces, "\n")
			if isError {
				text = "Tool error:\n" + text
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": text})
		default:
			return nil, wireError("unsupported history role")
		}
	}
	if len(pending) != 0 && !allowPending {
		return nil, wireError("historical calls are missing results")
	}
	tools := []any{}
	toolNames := map[string]bool{}
	for _, tool := range request.Tools {
		var schema map[string]any
		if tool.Name == "" || toolNames[tool.Name] || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, wireError("invalid function definition")
		}
		toolNames[tool.Name] = true
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "strict": false})
	}
	body := map[string]any{"model": request.Model, "instructions": request.System, "input": input, "tools": tools, "stream": true, "store": false, "truncation": "disabled", "max_output_tokens": request.MaxOutputTokens, "include": []string{"reasoning.encrypted_content"}}
	if effort != "" {
		body["reasoning"] = map[string]string{"effort": effort}
	}
	encoded, err := json.Marshal(body)
	if len(encoded) > 32<<20 {
		return nil, protocol.E("context_limit", "Responses request exceeds the transport budget")
	}
	return encoded, err
}
