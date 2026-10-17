package store

import (
	"context"
	"database/sql"
	"math"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// A preparation owns an immutable source boundary. It is not a published view
// or an accepted compaction operation. The coordinator must persist its intent,
// generate and check the summary, then publish through PublishContextView.
type CompactionPreparation struct {
	View           ContextView
	Policy         protocol.ContextBudgetPolicy
	EstimateMethod string
	SourceMessages int
	PrefixTokens   int64
	RetainedTokens int64
	FirstKept      int64
	LastKept       int64
	// Public source records for the summarizer, including resource/raw links.
	// Opaque replay data is excluded; the source journal retains it unchanged.
	SummaryInput []ContextMessage
	// The next ordinary model request must retain the original tail, including
	// its private protocol data. It must also read any tail appended meanwhile.
	Retained []model.Message
}

func (s *Store) PrepareCompaction(ctx context.Context, agent string, cursor int64, policy protocol.ContextBudgetPolicy) (CompactionPreparation, error) {
	var out CompactionPreparation
	if !policy.Valid() {
		return out, protocol.E("invalid_params", "compaction requires a valid retained-context policy")
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		boundary, err := contextQueryBoundary(ctx, tx, protocol.ContextTarget{AgentID: agent, At: &cursor})
		if err != nil {
			return err
		}
		entries, err := contextEntries(ctx, tx, agent, boundary.cursor)
		if err != nil {
			return err
		}
		var cuts []int
		complete, _, _, err := scanContextBoundaries(entries, func(cut int) { cuts = append(cuts, cut) })
		if err != nil {
			return err
		}
		if complete != len(entries) {
			return protocol.E("context_busy", "compaction must wait for complete tool call/result groups")
		}
		suffix := make([]int64, len(entries)+1)
		for i := len(entries) - 1; i >= 0; i-- {
			count, err := model.EstimateMessageTokens(entries[i].Record.Message)
			if err != nil {
				return err
			}
			if count > math.MaxInt64-suffix[i+1] {
				return protocol.E("context_limit", "context exceeds the compaction accounting range")
			}
			suffix[i] = suffix[i+1] + count
		}
		cut := 0
		for _, candidate := range cuts {
			// Retain at least the configured tail and one complete group. Do not
			// replace only an existing summary, which would make no raw progress.
			if candidate < len(entries) && entries[candidate-1].ViewID == "" && suffix[candidate] >= int64(policy.KeepRecentTokens) {
				cut = candidate
			}
		}
		if cut == 0 {
			return protocol.E("context_not_compactable", "no complete prefix can be summarized while retaining the configured recent context")
		}
		out.View = ContextView{AgentID: agent, Previous: boundary.view.ID, SourceCursor: cursor, Through: entries[cut-1].Seq}
		out.Policy, out.EstimateMethod, out.SourceMessages = policy, model.ContextEstimateMethod, len(entries)
		out.PrefixTokens, out.RetainedTokens = suffix[0]-suffix[cut], suffix[cut]
		out.FirstKept, out.LastKept = entries[cut].Seq, entries[len(entries)-1].Seq
		out.SummaryInput = make([]ContextMessage, 0, cut)
		for _, entry := range entries[:cut] {
			record := entry.Record
			record.Message.Opaque, record.Message.Protocol = nil, ""
			out.SummaryInput = append(out.SummaryInput, record)
		}
		out.Retained = make([]model.Message, 0, len(entries)-cut)
		for _, entry := range entries[cut:] {
			out.Retained = append(out.Retained, entry.Record.Message)
		}
		return nil
	})
	if err != nil {
		return CompactionPreparation{}, err
	}
	return out, nil
}
