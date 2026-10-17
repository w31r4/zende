package store

import (
	"context"
	"database/sql"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (s *Store) RecordContextBudgetBlock(ctx context.Context, agent, task string, revision, cursor int64, viewID string, budget protocol.ContextBudget) error {
	if !model.ValidContextBudget(budget) || budget.State != "over_capacity" && budget.State != "insufficient_capacity" {
		return protocol.E("invalid_params", "capacity refusal requires an over-limit or unusable budget")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := requestBoundary(ctx, tx, agent, task, revision, cursor); err != nil {
			return err
		}
		if err := manualCompactionBoundary(ctx, tx, agent); err != nil {
			return err
		}
		view, err := contextView(ctx, tx, agent, cursor)
		if err != nil {
			return err
		}
		if view.ID != viewID {
			return protocol.E("context_invalid", "capacity refusal view does not match its source boundary")
		}
		_, err = appendEvent(ctx, tx, agent, task, "context.budget.blocked", struct {
			Cursor   int64                  `json:"context_cursor"`
			Revision int64                  `json:"selection_revision"`
			ViewID   string                 `json:"context_view,omitempty"`
			Budget   protocol.ContextBudget `json:"budget"`
		}{cursor, revision, viewID, budget})
		return err
	})
}
