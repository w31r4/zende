package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// This is a selection heuristic, not provider usage or a context-window proof.
// Include structured/private fields and image overhead, not just visible text.
const contextEstimateMethod = "json-bytes/4+8-per-message+1200-per-image-v1"

func estimateContext(entries []contextEntry) int {
	tokens := 0
	for _, entry := range entries {
		data, _ := json.Marshal(entry.Record.Message) // messages originate from persisted JSON
		tokens += (len(data)+3)/4 + 8
		for _, b := range entry.Record.Message.Content {
			if b.Type == "image" {
				tokens += 1200
			}
		}
	}
	return tokens
}

// Returns safe cuts and the positions of real user turns. A user message sent
// during an open tool group belongs to that group; selecting it also retains
// the preceding turn needed to close the calls. Summaries are not user turns.
func contextCuts(entries []contextEntry) ([]int, []int, error) {
	complete, cuts, turns, err := scanContext(entries)
	if err != nil {
		return nil, nil, err
	}
	if complete != len(entries) {
		return nil, nil, protocol.E("context_invalid", "background contains unfinished tool calls")
	}
	return cuts, turns, nil
}

func scanContext(entries []contextEntry) (int, []int, []int, error) {
	return scanContextBoundaries(entries, nil)
}

// onComplete observes every closed message/tool group, not only user turns.
// Compaction can therefore shorten one long-running turn without splitting
// calls from results. Inheritance retains its existing user-turn cuts.
func scanContextBoundaries(entries []contextEntry, onComplete func(int)) (int, []int, []int, error) {
	pending, seen := map[string]bool{}, map[string]bool{}
	cuts, turns := []int{0}, []int{}
	complete := 0
	for i, entry := range entries {
		message := entry.Record.Message
		if message.Role == "user" && entry.Record.Summary == nil {
			turns = append(turns, i)
			if len(pending) == 0 && i > 0 {
				cuts = append(cuts, i)
			}
		}
		results := map[string]bool{}
		for _, b := range message.Content {
			if b.Type == "tool_call" {
				if message.Role != "assistant" || b.CallID == "" || seen[b.CallID] {
					return 0, nil, nil, protocol.E("context_invalid", "invalid inherited tool call")
				}
				pending[b.CallID], seen[b.CallID] = true, true
			}
			if message.Role == "tool" {
				if b.CallID == "" || !pending[b.CallID] {
					return 0, nil, nil, protocol.E("context_invalid", "orphan inherited tool result")
				}
				results[b.CallID] = true
			}
		}
		for id := range results {
			delete(pending, id)
		}
		if len(pending) == 0 {
			complete = i + 1
			if onComplete != nil {
				onComplete(complete)
			}
		}
	}
	return complete, cuts, turns, nil
}

func spawnContext(ctx context.Context, tx *sql.Tx, p protocol.SpawnAgent, source *protocol.SpawnSource) ([]contextEntry, error) {
	mode, last, err := protocol.SpawnContextMode(p.Context, p.ContextBudget)
	if err != nil {
		return nil, err
	}
	if len(p.Background) > 2<<20 || !utf8.ValidString(p.Background) || len(p.BackgroundHistory) > 16 {
		return nil, protocol.E("invalid_params", "background must be UTF-8 up to 2 MiB, with at most 16 history ranges")
	}
	source.Context, source.Budget, source.EstimateMethod = mode, p.ContextBudget, contextEstimateMethod
	view, err := contextView(ctx, tx, p.ParentID, source.ParentCursor)
	if err != nil {
		return nil, err
	}
	source.ViewID = view.ID
	var inherited []contextEntry
	if mode != "none" {
		inherited, err = contextEntries(ctx, tx, p.ParentID, source.ParentCursor)
		if err != nil {
			return nil, err
		}
		complete, err := completePrefix(inherited)
		if err != nil {
			return nil, err
		}
		source.ExcludedPending = len(inherited) - complete
		inherited = inherited[:complete]
	}
	var background []contextEntry
	for _, ref := range p.BackgroundHistory {
		if ref.AgentID == "" || ref.First <= 0 || ref.Last < ref.First {
			return nil, protocol.E("invalid_params", "background history requires an Agent and inclusive message endpoints")
		}
		entries, err := rawContextEntries(ctx, tx, ref.AgentID, ref.First-1, ref.Last)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 || entries[0].Seq != ref.First || entries[len(entries)-1].Seq != ref.Last {
			return nil, protocol.E("context_invalid", "background history endpoints must exist as context.message events")
		}
		if _, _, err := contextCuts(entries); err != nil {
			return nil, err
		}
		background = append(background, entries...)
	}
	if p.Background != "" {
		background = append(background, contextEntry{Record: ContextMessage{Message: model.Message{Role: "user", Content: []model.Block{{Type: "text", Text: "Additional task background:\n" + p.Background}}}}})
	}
	cuts, turns, err := contextCuts(inherited)
	if err != nil {
		return nil, err
	}
	start := 0
	if last > 0 && len(turns) > 0 {
		target := turns[max(0, len(turns)-last)]
		for _, cut := range cuts {
			if cut <= target {
				start = cut
			}
		}
	}
	backgroundTokens := estimateContext(background)
	suffixTokens := make([]int, len(inherited)+1)
	for i := len(inherited) - 1; i >= 0; i-- {
		suffixTokens[i] = suffixTokens[i+1] + estimateContext(inherited[i:i+1])
	}
	if mode == "budget" && len(inherited) > 0 {
		start = -1
		// Selection is a contiguous recent suffix. Never skip a costly new turn
		// to include an older one, or silently return no history when it exists.
		for _, cut := range cuts {
			if suffixTokens[cut]+backgroundTokens <= p.ContextBudget {
				start = cut
				break
			}
		}
		if start < 0 {
			minimum := suffixTokens[cuts[len(cuts)-1]] + backgroundTokens
			return nil, protocol.E("context_budget", fmt.Sprintf("newest complete turn and background need an estimated %d tokens; budget is %d", minimum, p.ContextBudget))
		}
	}
	selected := inherited[start:]
	source.EstimatedTokens = suffixTokens[start] + backgroundTokens
	if p.ContextBudget > 0 && source.EstimatedTokens > p.ContextBudget {
		return nil, protocol.E("context_budget", fmt.Sprintf("requested context and background need an estimated %d tokens; budget is %d; choose fewer turns or a larger budget", source.EstimatedTokens, p.ContextBudget))
	}
	source.ExcludedSelection = start
	source.Messages = len(selected)
	for _, turn := range turns {
		if turn >= start {
			source.Turns++
		}
	}
	for _, entry := range selected {
		if entry.ViewID != "" {
			source.SummaryIncluded = true
			continue
		}
		if source.First == 0 {
			source.First = entry.Seq
		}
		source.Last = entry.Seq
	}
	source.BackgroundMessages, source.BackgroundHistory = len(background), p.BackgroundHistory
	combined := append(append([]contextEntry{}, selected...), background...)
	// References may overlap selected context. Reject duplicate calls instead
	// of emitting a request the provider cannot interpret.
	if _, _, err := contextCuts(combined); err != nil {
		return nil, err
	}
	return combined, nil
}
