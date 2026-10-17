package responses

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const searchToolName = "tool_search"
const maxSearchTools = 50
const maxSearchDefinitions = 256

// Function arguments are JSON encoded inside a string. Native client search
// arguments are a JSON object. Keep those wire types distinct while delivering
// both through the common call/arguments/end events.
func (item wireItem) call(clientSearch bool) (string, string, error) {
	switch item.Type {
	case "function_call":
		if item.Execution != "" || item.Namespace != "" && (!clientSearch || item.Namespace != item.Name) {
			return "", "", protocol.E("unsupported_capability", "Responses function namespace or execution mode is not supported")
		}
		var arguments string
		if len(item.Arguments) != 0 && (bytes.Equal(item.Arguments, []byte("null")) || json.Unmarshal(item.Arguments, &arguments) != nil) {
			return "", "", wireError("function arguments must be a JSON string")
		}
		return item.Name, arguments, nil
	case "tool_search_call":
		if !clientSearch || item.Execution != "client" {
			return "", "", protocol.E("unsupported_capability", "Responses client tool search is not enabled")
		}
		if item.Name != "" || item.Namespace != "" {
			return "", "", wireError("native tool search has no function name or namespace")
		}
		return searchToolName, string(item.Arguments), nil
	default:
		return "", "", wireError("item is not a supported call")
	}
}

func searchName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name == searchToolName {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Convert only the result paired with a saved native search call. Ordinary
// tool/user text that happens to contain a tools array never loads definitions.
func searchOutput(callID, text string, failed bool, definitions map[string]model.Tool) ([]any, error) {
	if len(text) > model.MaxBlockBytes || !utf8.ValidString(text) {
		return nil, protocol.E("output_limit", "native tool search result exceeds the text budget")
	}
	tools := []any{}
	data := ""
	if failed {
		data = "Tool search failed; no tools were loaded.\n" + text
	} else {
		var result model.ToolSearchResult
		if protocol.StrictDecode([]byte(text), &result) != nil || result.Tools == nil {
			return nil, wireError("native tool search requires a complete tools result")
		}
		if len(result.Tools) > maxSearchTools {
			return nil, protocol.E("output_limit", "native tool search returned too many definitions; request a smaller page")
		}
		seen := map[string]bool{}
		for _, tool := range result.Tools {
			var schema map[string]any
			if !searchName(tool.Name) || seen[tool.Name] || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
				return nil, wireError("invalid discovered function definition")
			}
			seen[tool.Name] = true
			if prior, exists := definitions[tool.Name]; exists {
				if prior.Description != tool.Description || !sameJSON(prior.InputSchema, tool.InputSchema) {
					return nil, wireError("discovery changed a previously loaded tool; use an immutable definition name")
				}
			} else {
				if len(definitions) >= maxSearchDefinitions {
					return nil, protocol.E("context_limit", "native search definitions exceed the request limit")
				}
				definitions[tool.Name] = tool
			}
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "strict": false, "defer_loading": true})
		}
		data = result.Text
	}
	items := []any{map[string]any{"type": "tool_search_output", "execution": "client", "call_id": callID, "status": "completed", "tools": tools}}
	if data != "" {
		// The native output schema carries definitions, not pagination/error
		// text. Preserve that data in a neighboring user-level input item;
		// never promote an MCP description or result to developer instructions.
		metadata, err := json.Marshal(map[string]any{"type": "tool_search_result_data", "call_id": callID, "is_error": failed, "text": data})
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"role": "user", "content": string(metadata)})
	}
	return items, nil
}
