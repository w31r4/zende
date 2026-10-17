package anthropic

import (
	"encoding/json"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const searchToolName = "tool_search"
const maxSearchTools = 50
const maxSearchDefinitions = 256

func toolDefinition(tool model.Tool, deferred bool) map[string]any {
	definition := map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema}
	if deferred {
		definition["defer_loading"] = true
	}
	return definition
}

// Keep definitions in discovery order. The API expands them at tool_reference
// locations, while defer_loading excludes them from the system-prompt prefix.
// Never take definitions from arbitrary text or legacy host search results.
type deferredTools struct {
	reserved    map[string]bool
	definitions map[string]model.Tool
	order       []string
}

func (d *deferredTools) result(text string, failed bool) (any, error) {
	if len(text) > model.MaxBlockBytes || !utf8.ValidString(text) {
		return nil, protocol.E("output_limit", "native tool search result exceeds the text budget")
	}
	if failed {
		return text, nil // An error result never loads functions.
	}
	var result model.ToolSearchResult
	if protocol.StrictDecode([]byte(text), &result) != nil || result.Tools == nil {
		return nil, wireError("native tool search requires a complete tools result")
	}
	if len(result.Tools) > maxSearchTools {
		return nil, protocol.E("output_limit", "native tool search returned too many definitions; request a smaller page")
	}
	parts, seen := []any{}, map[string]bool{}
	for _, tool := range result.Tools {
		var schema map[string]any
		if !toolName.MatchString(tool.Name) || tool.Name == searchToolName || seen[tool.Name] || d.reserved[tool.Name] ||
			!utf8.ValidString(tool.Description) || decodeObject(tool.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, wireError("invalid or conflicting discovered function definition")
		}
		seen[tool.Name] = true
		if prior, exists := d.definitions[tool.Name]; exists {
			if prior.Description != tool.Description || !sameJSON(prior.InputSchema, tool.InputSchema) {
				return nil, wireError("discovery changed a previously loaded tool; use an immutable definition name")
			}
		} else {
			if len(d.reserved)+len(d.definitions) >= maxSearchDefinitions {
				return nil, protocol.E("context_limit", "native search definitions exceed the request limit")
			}
			d.definitions[tool.Name] = tool
			d.order = append(d.order, tool.Name)
		}
		parts = append(parts, map[string]string{"type": "tool_reference", "tool_name": tool.Name})
	}
	if result.Text != "" {
		parts = append(parts, map[string]string{"type": "text", "text": result.Text})
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]string{"type": "text", "text": "No matching tools."})
	}
	return parts, nil
}
