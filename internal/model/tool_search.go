package model

import "zende.local/harness/internal/protocol"

// ToolSearchResult is the portable result of a host-executed native search.
// The host retains it as a JSON text tool result, paired with the original
// call. An adapter converts Tools to its native discovery representation.
// Text carries bounded search metadata (for example pagination) as data, not
// instructions. Executors and their immutable bindings remain host-owned.
type ToolSearchResult struct {
	Tools []Tool `json:"tools"`
	Text  string `json:"text,omitempty"`
}

// NativeSearchImports is the set of source-validated calls a native adapter
// must re-encode. Delete an ID only when matching a public tool_search call;
// reject any unconsumed IDs after encoding the complete history.
func NativeSearchImports(request Request, enabled bool) (map[string]bool, error) {
	if len(request.NativeSearchCalls) != 0 && !enabled {
		return nil, protocol.E("unsupported_capability", "native search history conversion requires an enabled native route")
	}
	if len(request.NativeSearchCalls) > len(request.Messages) {
		return nil, protocol.E("context_invalid", "native search imports exceed the source message count")
	}
	imports := map[string]bool{}
	for _, id := range request.NativeSearchCalls {
		if id == "" || len(id) > 512 || imports[id] {
			return nil, protocol.E("context_invalid", "native search imports have invalid or repeated call IDs")
		}
		imports[id] = true
	}
	return imports, nil
}
