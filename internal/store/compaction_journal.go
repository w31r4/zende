package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const maxCompactionDeltas = 32768
const maxCompactionDeltaBytes = 8 << 20

type compactionDelta struct {
	OperationID string      `json:"operation_id"`
	RequestID   string      `json:"request_id"`
	Index       int         `json:"index"`
	Delta       model.Delta `json:"delta"`
	TextBytes   []byte      `json:"text_bytes,omitempty"`
}

// Raw deltas include provider-private continuation evidence, so this journal is
// not a public progress feed. Public progress uses context.compaction.state.
func (s *Store) RecordCompactionDelta(ctx context.Context, id, owner, request string, delta model.Delta) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedCompaction(ctx, tx, id, owner, request)
		if err != nil {
			return err
		}
		d := compactionDelta{OperationID: id, RequestID: request, Index: r.DeltaCount + 1, Delta: delta}
		// Adapters may split a UTF-8 code point across text fragments. JSON
		// strings alone would replace those bytes and corrupt replay.
		if !utf8.ValidString(delta.Text) {
			d.TextBytes = []byte(delta.Text)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return protocol.E("summary_invalid", "summary delta cannot be encoded")
		}
		if r.DeltaCount >= maxCompactionDeltas || len(raw) > maxCompactionDeltaBytes-r.DeltaBytes {
			return protocol.E("summary_limit", "summary stream exceeded its durable event budget")
		}
		if _, err := appendEvent(ctx, tx, r.AgentID, "", "context.compaction.delta", d); err != nil {
			return err
		}
		r.DeltaCount++
		r.DeltaBytes += len(raw)
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE compactions SET record=? WHERE id=?", string(data), id)
		return err
	})
}

func compactionResponse(ctx context.Context, tx *sql.Tx, r CompactionRecord) (model.Response, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND seq>? AND kind='context.compaction.delta'
AND json_extract(payload,'$.operation_id')=? AND json_extract(payload,'$.request_id')=? ORDER BY seq`, r.AgentID, r.StartedSeq, r.OperationID, r.RequestID)
	if err != nil {
		return model.Response{}, err
	}
	defer rows.Close()
	var collector model.Collector
	var count, size int
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return model.Response{}, err
		}
		count++
		size += len(raw)
		var d compactionDelta
		if count > r.DeltaCount || size > r.DeltaBytes || protocol.StrictDecode(raw, &d) != nil || d.OperationID != r.OperationID || d.RequestID != r.RequestID || d.Index != count {
			return model.Response{}, protocol.E("invalid_state", "summary stream journal has a gap or mismatched identity")
		}
		if d.TextBytes != nil {
			d.Delta.Text = string(d.TextBytes)
		}
		if err := collector.Add(d.Delta); err != nil {
			return model.Response{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return model.Response{}, err
	}
	if count != r.DeltaCount || size != r.DeltaBytes {
		return model.Response{}, protocol.E("invalid_state", "summary stream journal is incomplete")
	}
	return collector.Response()
}

// Event subscribers need progress, not a second copy of the whole transcript
// or private response fragments. Keep event seq/kind unchanged for replay.
func publicCompactionEvent(event protocol.Event) (protocol.Event, error) {
	var payload any
	switch event.Kind {
	case "context.compaction.started":
		var a CompactionAttempt
		if err := json.Unmarshal(event.Payload, &a); err != nil {
			return event, err
		}
		payload = struct {
			OperationID  string                 `json:"operation_id"`
			RequestID    string                 `json:"request_id"`
			Attempt      int                    `json:"attempt"`
			Model        string                 `json:"model"`
			Protocol     string                 `json:"protocol"`
			SourceCursor int64                  `json:"source_cursor"`
			Through      int64                  `json:"through"`
			Previous     string                 `json:"previous_view,omitempty"`
			FirstKept    int64                  `json:"first_kept"`
			LastKept     int64                  `json:"last_kept"`
			Budget       protocol.ContextBudget `json:"budget"`
		}{a.OperationID, a.Request.ID, a.Number, a.Request.Model, a.Protocol, a.View.SourceCursor, a.View.Through, a.View.Previous, a.FirstKept, a.LastKept, a.Budget}
	case "context.compaction.delta":
		var d compactionDelta
		if err := json.Unmarshal(event.Payload, &d); err != nil {
			return event, err
		}
		payload = struct {
			OperationID string `json:"operation_id"`
			RequestID   string `json:"request_id"`
			Index       int    `json:"index"`
			Kind        string `json:"kind"`
		}{d.OperationID, d.RequestID, d.Index, d.Delta.Kind}
	default:
		return event, nil
	}
	data, err := json.Marshal(payload)
	event.Payload = data
	return event, err
}

// CompleteCompactionAttempt is called only after Stream returned nil. A saved
// response.end alone cannot establish transport success after a Core crash.
// Failure rolls back the view, completion event and projection together.
func (s *Store) CompleteCompactionAttempt(ctx context.Context, id, owner, request string) (ContextView, error) {
	var published ContextView
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedCompaction(ctx, tx, id, owner, request)
		if err != nil {
			return err
		}
		a, err := loadCompactionAttempt(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := treeWritable(ctx, tx, r.AgentID); err != nil {
			return err
		}
		if a.TaskID != "" {
			if err := activeTask(ctx, tx, r.AgentID, a.TaskID); err != nil {
				return err
			}
		} else {
			var state string
			if err := tx.QueryRowContext(ctx, "SELECT state FROM agents WHERE id=?", r.AgentID).Scan(&state); err != nil {
				return err
			}
			if state != "idle" && state != "paused" {
				return protocol.E("context_busy", "Agent started work during summary generation")
			}
		}
		response, err := compactionResponse(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := response.ValidateProtocol(a.Protocol); err != nil {
			return protocol.E("summary_invalid", "summary protocol differs from its saved binding")
		}
		summary, err := model.CompactionSummary(response)
		if err != nil {
			return err
		}
		view := a.View
		view.Summary = summary
		// Include the host-owned raw-reference wrapper in the shrink check.
		// publishContextView assigns another ID with the same fixed length.
		view.ID = protocol.NewID("view")
		tokens, err := model.EstimateMessageTokens(view.message())
		if err != nil {
			return err
		}
		if tokens >= a.PrefixTokens {
			return protocol.E("summary_not_smaller", "summary does not reduce the selected prefix")
		}
		published, err = publishContextView(ctx, tx, r.AgentID, view)
		if err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, r.AgentID, a.TaskID, "context.compaction.completed", struct {
			OperationID    string       `json:"operation_id"`
			RequestID      string       `json:"request_id"`
			ViewID         string       `json:"view_id"`
			PrefixTokens   int64        `json:"prefix_tokens"`
			SummaryTokens  int64        `json:"summary_tokens"`
			EstimateMethod string       `json:"estimate_method"`
			Usage          *model.Usage `json:"usage,omitempty"`
		}{id, request, published.ID, a.PrefixTokens, tokens, model.ContextEstimateMethod, response.Usage}); err != nil {
			return err
		}
		r.State, r.ViewID, r.Failure = "completed", published.ID, nil
		return saveCompaction(ctx, tx, r)
	})
	if err != nil {
		return ContextView{}, err
	}
	return published, nil
}

func cancelAgentCompaction(ctx context.Context, tx *sql.Tx, agent string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM compactions WHERE agent_id=? AND json_extract(record,'$.state') IN ('pending','running')`, agent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := compactionRecord(ctx, tx, id)
	if err != nil {
		return err
	}
	return finishCompactionFailure(ctx, tx, r, "cancelled", false)
}
