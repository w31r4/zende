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

const compactionSchema = `
CREATE TABLE compactions (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), record TEXT NOT NULL
);
CREATE UNIQUE INDEX compaction_active ON compactions(agent_id)
 WHERE json_extract(record,'$.state') IN ('pending','running');
PRAGMA user_version=17;
`

// Only this projection changes. Every transition and attempt remains in events.
type CompactionRecord struct {
	protocol.CompactionInfo
	Policy     protocol.ContextBudgetPolicy `json:"policy"`
	Owner      string                       `json:"owner,omitempty"`
	StartedSeq int64                        `json:"started_seq,omitempty"`
	DeltaCount int                          `json:"delta_count"`
	DeltaBytes int                          `json:"delta_bytes"`
	Dispatched bool                         `json:"dispatched,omitempty"`
}

type CompactionAttempt struct {
	OperationID       string                 `json:"operation_id"`
	Number            int                    `json:"number"`
	Owner             string                 `json:"owner"`
	TaskID            string                 `json:"task_id,omitempty"`
	SelectionRevision int64                  `json:"selection_revision"`
	Protocol          string                 `json:"protocol"`
	Binding           json.RawMessage        `json:"binding,omitempty"`
	PromptVersion     int                    `json:"prompt_version"`
	Request           model.Request          `json:"request"`
	Budget            protocol.ContextBudget `json:"budget"`
	View              ContextView            `json:"view"`
	SourceMessages    int                    `json:"source_messages"`
	PrefixTokens      int64                  `json:"prefix_tokens"`
	RetainedTokens    int64                  `json:"retained_tokens"`
	FirstKept         int64                  `json:"first_kept"`
	LastKept          int64                  `json:"last_kept"`
}

func compactionIdentity(s string) bool { return s != "" && len(s) <= 128 && utf8.ValidString(s) }

func compactionRecord(ctx context.Context, q rowQuerier, id string) (CompactionRecord, error) {
	var r CompactionRecord
	var agent string
	var raw []byte
	err := q.QueryRowContext(ctx, "SELECT agent_id,record FROM compactions WHERE id=?", id).Scan(&agent, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, protocol.E("not_found", "compaction operation not found")
	}
	if err != nil {
		return r, err
	}
	if len(raw) > 32<<10 || protocol.StrictDecode(raw, &r) != nil || r.OperationID != id || r.AgentID != agent || !r.Policy.Valid() || r.MaxAttempts != r.Policy.MaxAttempts || r.Attempts < 0 || r.Attempts > r.MaxAttempts || r.DeltaCount < 0 || r.DeltaCount > maxCompactionDeltas || r.DeltaBytes < 0 || r.DeltaBytes > maxCompactionDeltaBytes {
		return r, protocol.E("invalid_state", "compaction projection is unreadable")
	}
	if r.Reason != "manual" && r.Reason != "automatic" {
		return r, protocol.E("invalid_state", "compaction reason is invalid")
	}
	switch r.State {
	case "pending":
		if r.Attempts >= r.MaxAttempts {
			return r, protocol.E("invalid_state", "compaction retry budget is exhausted")
		}
	case "running":
		if r.Owner == "" || r.RequestID == "" || r.Attempts == 0 || r.StartedSeq <= 0 || r.Failure != nil {
			return r, protocol.E("invalid_state", "running compaction has no complete attempt identity")
		}
	case "completed":
		if r.ViewID == "" || r.Failure != nil {
			return r, protocol.E("invalid_state", "completed compaction has no view")
		}
	case "failed", "cancelled":
		if r.Failure == nil {
			return r, protocol.E("invalid_state", "terminal compaction has no failure reason")
		}
	default:
		return r, protocol.E("invalid_state", "compaction state is invalid")
	}
	return r, nil
}

func saveCompaction(ctx context.Context, tx *sql.Tx, r CompactionRecord) error {
	if _, err := appendEvent(ctx, tx, r.AgentID, "", "context.compaction.state", struct {
		protocol.CompactionInfo
		Policy protocol.ContextBudgetPolicy `json:"policy"`
	}{r.CompactionInfo, r.Policy}); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO compactions(id,agent_id,record) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET record=excluded.record`, r.OperationID, r.AgentID, string(raw))
	return err
}

// QueueCompaction accepts intent only. The scheduler must wait for complete
// tool groups and persist BeginCompactionAttempt before contacting a model.
func (s *Store) QueueCompaction(ctx context.Context, req protocol.Request, agent, task, reason string, policy protocol.ContextBudgetPolicy) (protocol.Accepted, error) {
	var target protocol.CompactContext
	if req.Validate() != nil || req.Method != "context.compact" || protocol.StrictDecode(req.Params, &target) != nil || target.AgentID != agent || !compactionIdentity(agent) || !compactionIdentity(req.OperationID) || !policy.Valid() || reason != "manual" && reason != "automatic" || (reason == "automatic") != (task != "") {
		return protocol.Accepted{}, protocol.E("invalid_params", "compaction requires an Agent, operation identity and valid policy")
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := resourceAgent(ctx, tx, agent, true); err != nil {
			return protocol.Accepted{}, err
		}
		if err := treeWritable(ctx, tx, agent); err != nil {
			return protocol.Accepted{}, err
		}
		if task != "" {
			if err := activeTask(ctx, tx, agent, task); err != nil {
				return protocol.Accepted{}, err
			}
		}
		if err := treeCallerWritable(ctx, tx, target.Caller); err != nil {
			return protocol.Accepted{}, err
		}
		source, err := callSource(ctx, tx, target.Caller)
		if err != nil {
			return protocol.Accepted{}, err
		}
		var active string
		err = tx.QueryRowContext(ctx, `SELECT id FROM compactions WHERE agent_id=? AND json_extract(record,'$.state') IN ('pending','running')`, agent).Scan(&active)
		if err == nil {
			failure := protocol.E("compaction_busy", "Agent already has a pending compaction; inspect the existing operation")
			failure.Data.OperationID = active
			return protocol.Accepted{}, failure
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, err
		}
		r := CompactionRecord{CompactionInfo: protocol.CompactionInfo{OperationID: req.OperationID, AgentID: agent, TaskID: task, Reason: reason, State: "pending", MaxAttempts: policy.MaxAttempts, Source: source}, Policy: policy}
		if err := saveCompaction(ctx, tx, r); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: agent, TaskID: task}, nil
	})
}

func (s *Store) Compaction(ctx context.Context, id string) (CompactionRecord, error) {
	return compactionRecord(ctx, s.db, id)
}

// A summary may be generated for a paused idle Agent without resuming it.
// Active Tasks use their own single-loop gate, and unknown effects stay held.
func compactionBoundary(ctx context.Context, tx *sql.Tx, agent, task string, cursor, revision int64) error {
	if err := resourceAgent(ctx, tx, agent, true); err != nil {
		return err
	}
	if err := treeWritable(ctx, tx, agent); err != nil {
		return err
	}
	if task != "" {
		_, err := requestBoundary(ctx, tx, agent, task, revision, cursor)
		return err
	}
	var state string
	var seq int64
	if err := tx.QueryRowContext(ctx, "SELECT state,seq FROM agents WHERE id=?", agent).Scan(&state, &seq); err != nil {
		return err
	}
	if state != "idle" && state != "paused" {
		return protocol.E("context_busy", "compaction must wait for the active Task to settle")
	}
	choice, err := selection(ctx, tx, agent)
	if err != nil {
		return err
	}
	if seq != cursor || choice.Choice.Revision != revision {
		return protocol.E("request_conflict", "context or selection changed before summary preparation")
	}
	return nil
}

func (s *Store) BeginCompactionAttempt(ctx context.Context, id, owner, task string, cursor, revision int64, binding model.Binding) (CompactionAttempt, error) {
	var attempt CompactionAttempt
	if !compactionIdentity(owner) || binding.Model == "" || binding.Protocol == "" || binding.MaxOutputTokens <= 0 || binding.ContextWindow < 0 || len(binding.Snapshot) > 256<<10 || len(binding.Snapshot) > 0 && !json.Valid(binding.Snapshot) {
		return attempt, protocol.E("invalid_params", "summary requires an owner and a valid model binding")
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := compactionRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.State != "pending" || r.Attempts >= r.MaxAttempts {
			return protocol.E("compaction_conflict", "compaction is not awaiting another attempt")
		}
		if r.Reason == "automatic" && (r.TaskID == "" || r.TaskID != task) {
			return protocol.E("compaction_conflict", "automatic compaction belongs to another Task")
		}
		if err := compactionBoundary(ctx, tx, r.AgentID, task, cursor, revision); err != nil {
			return err
		}
		plan, err := prepareCompaction(ctx, tx, r.AgentID, cursor, r.Policy)
		if err != nil {
			return err
		}
		transcript, err := json.Marshal(struct {
			Agent    string           `json:"agent_id"`
			Cursor   int64            `json:"source_cursor"`
			Through  int64            `json:"through"`
			Previous string           `json:"previous_view,omitempty"`
			Records  []ContextMessage `json:"records"`
		}{r.AgentID, cursor, plan.View.Through, plan.View.Previous, plan.SummaryInput})
		if err != nil {
			return err
		}
		request, err := model.CompactionRequest(protocol.NewID("summary"), binding.Model, transcript, min(r.Policy.SummaryTokens, binding.MaxOutputTokens))
		if err != nil {
			return err
		}
		estimate, err := model.EstimateRequest(request)
		if err != nil {
			return err
		}
		budget, err := model.PlanContextBudget(estimate, int64(binding.ContextWindow), int64(request.MaxOutputTokens), r.Policy)
		if err != nil {
			return err
		}
		if budget.InputLimit != nil && (*budget.InputLimit <= 0 || estimate.InputTokens > *budget.InputLimit) {
			return protocol.E("context_capacity", "summary request exceeds declared model capacity; history is unchanged")
		}
		attempt = CompactionAttempt{OperationID: id, Number: r.Attempts + 1, Owner: owner, TaskID: task, SelectionRevision: revision,
			Protocol: binding.Protocol, Binding: append(json.RawMessage(nil), binding.Snapshot...), PromptVersion: model.CompactionPromptVersion,
			Request: request, Budget: budget, View: plan.View, SourceMessages: plan.SourceMessages,
			PrefixTokens: plan.PrefixTokens, RetainedTokens: plan.RetainedTokens, FirstKept: plan.FirstKept, LastKept: plan.LastKept}
		raw, err := json.Marshal(attempt)
		if err != nil {
			return err
		}
		if len(raw) > protocol.MaxFrameBytes {
			return protocol.E("context_limit", "summary attempt exceeds the durable record budget")
		}
		event, err := appendEvent(ctx, tx, r.AgentID, task, "context.compaction.started", attempt)
		if err != nil {
			return err
		}
		r.State, r.Owner, r.RequestID, r.StartedSeq = "running", owner, request.ID, event.Seq
		r.Attempts, r.DeltaCount, r.DeltaBytes, r.Failure = attempt.Number, 0, 0, nil
		r.Dispatched = false
		return saveCompaction(ctx, tx, r)
	})
	if err != nil {
		return CompactionAttempt{}, err
	}
	return attempt, nil
}

func ownedCompaction(ctx context.Context, tx *sql.Tx, id, owner, request string) (CompactionRecord, error) {
	r, err := compactionRecord(ctx, tx, id)
	if err == nil && (r.State != "running" || r.Owner != owner || r.RequestID != request) {
		err = protocol.E("compaction_conflict", "summary attempt no longer owns this operation")
	}
	return r, err
}

func loadCompactionAttempt(ctx context.Context, tx *sql.Tx, r CompactionRecord) (CompactionAttempt, error) {
	var a CompactionAttempt
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT payload FROM events WHERE agent_id=? AND seq=? AND kind='context.compaction.started'", r.AgentID, r.StartedSeq).Scan(&raw)
	if err != nil {
		return a, err
	}
	if len(raw) > protocol.MaxFrameBytes || protocol.StrictDecode(raw, &a) != nil || a.OperationID != r.OperationID || a.Number != r.Attempts || a.Owner != r.Owner || a.Request.ID != r.RequestID || a.View.AgentID != r.AgentID || a.View.SourceCursor >= r.StartedSeq || a.View.Through <= 0 || a.View.Through > a.View.SourceCursor || a.View.ID != "" || a.View.Summary != "" || a.PrefixTokens <= 0 || a.FirstKept <= a.View.Through || a.LastKept < a.FirstKept || a.LastKept > a.View.SourceCursor || a.PromptVersion != model.CompactionPromptVersion || a.Request.System != model.CompactionPrompt || len(a.Request.Tools) != 0 || !model.ValidContextBudget(a.Budget) || a.Budget.Policy != r.Policy || a.Budget.OutputReserve != int64(a.Request.MaxOutputTokens) {
		return a, protocol.E("invalid_state", "saved summary attempt does not match its operation")
	}
	estimate, err := model.EstimateRequest(a.Request)
	if err != nil || estimate != a.Budget.Estimate {
		return a, protocol.E("invalid_state", "saved summary request differs from its budget")
	}
	return a, nil
}

func compactionFailure(reason string) *protocol.Error {
	messages := map[string]string{
		"summary_invalid":         "Summary was incomplete or invalid; the previous view is preserved.",
		"summary_limit":           "Summary output exceeded its retained limit; the previous view is preserved.",
		"summary_not_smaller":     "Summary did not reduce the selected prefix; the previous view is preserved.",
		"context_capacity":        "Summary cannot fit the declared capacity; change capacity or retention settings and retry.",
		"context_not_compactable": "No complete prefix can be summarized with the configured retained tail.",
		"context_conflict":        "Context or selection changed during summary preparation or publication; inspect and retry.",
		"model_unavailable":       "Summary model or its saved binding is unavailable.",
		"model_failed":            "Summary request failed; the previous view and received fragments are preserved.",
		"interrupted":             "Core stopped during summary generation; the consumed attempt remains counted.",
		"cancelled":               "Summary operation was cancelled; the previous view is preserved.",
	}
	if message, ok := messages[reason]; ok {
		return protocol.E(reason, message)
	}
	return nil
}

func finishCompactionFailure(ctx context.Context, tx *sql.Tx, r CompactionRecord, reason string, retry bool) error {
	failure := compactionFailure(reason)
	if failure == nil {
		return protocol.E("invalid_params", "unsupported compaction failure reason")
	}
	failure.Data.OperationID = r.OperationID
	r.State, r.Failure = "failed", failure
	if reason == "cancelled" {
		r.State = "cancelled"
	} else if retry && r.Attempts < r.MaxAttempts {
		r.State = "pending"
	}
	return saveCompaction(ctx, tx, r)
}

func (s *Store) FailCompactionAttempt(ctx context.Context, id, owner, request, reason string, retry bool) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := ownedCompaction(ctx, tx, id, owner, request)
		if err != nil {
			return err
		}
		return finishCompactionFailure(ctx, tx, r, reason, retry)
	})
}

// Preflight failure consumes no model attempt, but must not leave a permanently
// pending operation that the scheduler will retry without any bound.
func (s *Store) FailPendingCompaction(ctx context.Context, id, reason string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := compactionRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.State != "pending" {
			return protocol.E("compaction_conflict", "compaction is no longer pending")
		}
		return finishCompactionFailure(ctx, tx, r, reason, false)
	})
}

// Called only by the elected Core at startup. Read-only summary requests may
// retry, but each interrupted request retains its identity and spent attempt.
func (s *Store) InterruptCompactions(ctx context.Context, owner string) error {
	if !compactionIdentity(owner) {
		return protocol.E("invalid_params", "Core owner is required")
	}
	var after string
	for {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM compactions WHERE id>? AND json_extract(record,'$.state')='running' AND json_extract(record,'$.owner')<>? ORDER BY id LIMIT 1`, after, owner).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.Transaction(ctx, func(tx *sql.Tx) error {
			r, err := compactionRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			if r.State != "running" || r.Owner == owner {
				return nil
			}
			return finishCompactionFailure(ctx, tx, r, "interrupted", true)
		}); err != nil {
			return err
		}
		after = id
	}
}

// Runtime calls this only after it has exclusively reclaimed a held Task and
// settled its previous tools. It also covers a failed worker in the same Core,
// which the startup-only owner check deliberately leaves alone.
func (s *Store) InterruptTaskCompaction(ctx context.Context, agent, task string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := activeTask(ctx, tx, agent, task); err != nil {
			return err
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM compactions WHERE agent_id=? AND json_extract(record,'$.state')='running'`, agent).Scan(&id)
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
		attempt, err := loadCompactionAttempt(ctx, tx, r)
		if err != nil {
			return err
		}
		if attempt.TaskID != task {
			return protocol.E("compaction_conflict", "running summary belongs to another worker")
		}
		return finishCompactionFailure(ctx, tx, r, "interrupted", true)
	})
}
