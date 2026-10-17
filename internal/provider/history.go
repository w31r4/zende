package provider

import (
	"sort"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/anthropic"
	"zende.local/harness/internal/model/chatcompletions"
	"zende.local/harness/internal/model/gemini"
	"zende.local/harness/internal/model/responses"
	"zende.local/harness/internal/protocol"
)

// PublicHistory is a detached request projection. The caller must explicitly
// select and persist its policy before using it; this function never edits
// stored history or silently falls back from strict protocol continuation.
type PublicHistory struct {
	Messages          []model.Message
	NativeSearchCalls []string
	SourceProtocols   []string
	Converted         int
	OmittedPrivate    int
	CategoryMarkers   int
}

// ProjectPublicHistory preserves private envelopes usable by the target and
// asks each other source protocol to validate/export its public content. The
// target must still validate the whole sequence, including call/result pairs.
// NativeSearchCalls must be carried into native transport re-encoding; callers
// must not discard this semantic information and claim full compatibility.
func ProjectPublicHistory(target model.Binding, messages []model.Message) (PublicHistory, error) {
	result := PublicHistory{Messages: []model.Message{}, NativeSearchCalls: []string{}, SourceProtocols: []string{}}
	if target.Adapter == nil || target.Model == "" || target.Protocol == "" {
		return PublicHistory{}, protocol.E("model_unavailable", "context conversion requires a selected adapter and model")
	}
	matcher, canMatch := target.Adapter.(model.PrivateHistoryMatcher)
	sources, searches := map[string]bool{}, map[string]bool{}
	for _, original := range messages {
		message := model.CloneMessage(original)
		if original.Protocol != "" || len(original.Opaque) != 0 {
			keep := canMatch && matcher.KeepsPrivateHistory(original, target.Model) && (original.Category == "" || target.NativeCategories)
			if !keep {
				exported, err := exportPublic(original)
				if err != nil {
					return PublicHistory{}, err
				}
				sources[original.Protocol] = true
				result.Converted++
				if exported.Message == nil {
					result.OmittedPrivate++
					continue
				}
				message = *exported.Message
				if id := exported.NativeSearchCallID; id != "" {
					if searches[id] {
						return PublicHistory{}, protocol.E("context_invalid", "converted native search IDs are not unique")
					}
					searches[id] = true
					result.NativeSearchCalls = append(result.NativeSearchCalls, id)
				}
			}
		}
		if message.Category != "" {
			if message.Role != "assistant" || message.Category != "commentary" && message.Category != "final" {
				return PublicHistory{}, protocol.E("context_conversion_unsupported", "historical output category cannot be converted")
			}
			if !target.NativeCategories {
				var text strings.Builder
				for _, block := range message.Content {
					if block.Type != "text" || block.Attachment != "" || block.CallID != "" || block.Name != "" || len(block.Arguments) != 0 || block.IsError {
						return PublicHistory{}, protocol.E("context_conversion_unsupported", "categorized history contains non-text content")
					}
					text.WriteString(block.Text)
				}
				message.Content = []model.Block{{Type: "text", Text: "<" + message.Category + ">\n" + text.String() + "\n</" + message.Category + ">"}}
				message.Category = ""
				result.CategoryMarkers++
			}
		}
		result.Messages = append(result.Messages, message)
	}
	for source := range sources {
		result.SourceProtocols = append(result.SourceProtocols, source)
	}
	sort.Strings(result.SourceProtocols)
	return result, nil
}

func exportPublic(message model.Message) (model.PublicExport, error) {
	if message.Role != "assistant" || len(message.Opaque) == 0 {
		return model.PublicExport{}, protocol.E("context_conversion_unsupported", "only recognized assistant protocol envelopes can be converted; original history is retained")
	}
	switch message.Protocol {
	case responses.Protocol:
		return responses.ExportPublic(message)
	case chatcompletions.Protocol:
		return chatcompletions.ExportPublic(message)
	case anthropic.Protocol:
		return anthropic.ExportPublic(message)
	case gemini.Protocol:
		return gemini.ExportPublic(message)
	default:
		return model.PublicExport{}, protocol.E("context_conversion_unsupported", "source protocol has no public-history exporter; original history is retained")
	}
}
