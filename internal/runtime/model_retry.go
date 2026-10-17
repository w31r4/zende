package runtime

import (
	"context"
	"errors"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type modelRetrySchedule struct {
	RequestID string          `json:"request_id"`
	Reason    string          `json:"reason"`
	Attempt   int             `json:"attempt"`
	Limit     int             `json:"limit"`
	DelayMS   int64           `json:"delay_ms"`
	Error     *protocol.Error `json:"error"`
}

type modelRetryDispatch struct {
	RequestID string `json:"request_id"`
	Attempt   int    `json:"attempt"`
}

type modelRetryState struct {
	Policy *model.RetryPolicy
	Count  int
}

func retryableModelHTTP(err error) (*protocol.Error, bool) {
	var failure *protocol.Error
	if !errors.As(err, &failure) || !failure.Data.Retryable {
		return nil, false
	}
	switch failure.Data.Kind {
	case "model_http", "model_rate_limit", "model_timeout":
		return failure, true
	}
	return nil, false
}

func waitModelDelay(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Reserve the attempt before waiting. A crash or cancellation never refunds
// it, and neither configuration publication nor model selection resets it.
func (r *modelRetryState) schedule(ctx context.Context, request, reason string, failure *protocol.Error, appendEvent func(string, any) error) error {
	if r.Policy == nil {
		return nil // Legacy Tasks have no saved automatic retry policy.
	}
	if !r.Policy.Valid() || r.Count < 0 || r.Count > r.Policy.Limit {
		return recoveryInvalid()
	}
	if r.Count >= r.Policy.Limit {
		if err := appendEvent("model.retry.exhausted", map[string]any{"request_id": request, "attempts": r.Count, "limit": r.Policy.Limit, "error": failure}); err != nil {
			return err
		}
		message := "model retry budget exhausted; history and completed tool results are preserved"
		if failure != nil {
			message = failure.Message + "; " + message
		}
		return protocol.E("model_retry_exhausted", message)
	}
	next := r.Count + 1
	delay := r.Policy.Delay(next)
	record := modelRetrySchedule{RequestID: request, Reason: reason, Attempt: next, Limit: r.Policy.Limit, DelayMS: delay.Milliseconds(), Error: failure}
	if err := appendEvent("model.retry.scheduled", record); err != nil {
		return err
	}
	r.Count = next
	if err := waitModelDelay(ctx, delay); err != nil {
		return err
	}
	// This event is fenced by activeTask again, after the cancellable wait.
	return appendEvent("model.retry.dispatching", modelRetryDispatch{RequestID: request, Attempt: next})
}

// Only explicit pre-stream HTTP failures may repeat this exact request. Any
// received delta, unknown transport outcome, or plugin provider exits here.
func (r *modelRetryState) stream(ctx context.Context, adapter model.Adapter, request model.Request, plugin bool, emit func(model.Delta) error, appendEvent func(string, any) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		received := false
		err := adapter.Stream(ctx, request, func(delta model.Delta) error {
			received = true
			return emit(delta)
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failure, retry := retryableModelHTTP(err)
		if err == nil || received || plugin || !retry || r.Policy == nil || r.Policy.Limit == 0 {
			return err
		}
		if err := r.schedule(ctx, request.ID, "http", failure, appendEvent); err != nil {
			return err
		}
	}
}
