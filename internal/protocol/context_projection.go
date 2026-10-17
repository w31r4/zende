package protocol

// Describes the request copy, never a rewrite of stored messages. Counts have
// no private text, endpoints, signatures or credential references.
type ContextProjection struct {
	Version           int      `json:"version"`
	SourceProtocols   []string `json:"source_protocols"`
	Converted         int      `json:"converted"`
	OmittedPrivate    int      `json:"omitted_private"`
	CategoryMarkers   int      `json:"category_markers"`
	NativeSearchCalls int      `json:"native_search_calls"`
}

func (p ContextProjection) Valid() bool {
	if p.Version != 1 || p.Converted < 0 || p.OmittedPrivate < 0 || p.OmittedPrivate > p.Converted || p.CategoryMarkers < 0 || p.NativeSearchCalls < 0 || p.NativeSearchCalls > p.Converted-p.OmittedPrivate || (p.Converted == 0) != (len(p.SourceProtocols) == 0) || len(p.SourceProtocols) > p.Converted {
		return false
	}
	previous := ""
	for _, source := range p.SourceProtocols {
		if source <= previous {
			return false
		}
		switch source {
		case "responses", "chat-completions", "anthropic", "gemini":
		default:
			return false
		}
		previous = source
	}
	return true
}
