package model

// ToolSearchResult is the portable result of a host-executed native search.
// The host retains it as a JSON text tool result, paired with the original
// call. An adapter converts Tools to its native discovery representation.
// Text carries bounded search metadata (for example pagination) as data, not
// instructions. Executors and their immutable bindings remain host-owned.
type ToolSearchResult struct {
	Tools []Tool `json:"tools"`
	Text  string `json:"text,omitempty"`
}
