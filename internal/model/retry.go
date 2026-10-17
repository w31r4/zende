package model

import (
	"fmt"
	"time"

	"zende.local/harness/internal/protocol"
)

// RetryPolicy is saved with a Task, independently of output-format repair.
// Delay saturates before conversion so large configured integers cannot wrap.
type RetryPolicy struct {
	Limit  int `json:"limit"`
	BaseMS int `json:"base_ms"`
}

func (p RetryPolicy) Valid() bool { return p.Limit >= 0 && p.BaseMS > 0 }

func (p RetryPolicy) Delay(attempt int) time.Duration {
	if !p.Valid() || attempt < 1 {
		return 0
	}
	const maximum = 30000
	delay := min(p.BaseMS, maximum)
	for n := 1; n < attempt && delay < maximum; n++ {
		if delay > maximum/2 {
			delay = maximum
		} else {
			delay *= 2
		}
	}
	return time.Duration(delay) * time.Millisecond
}

// HTTPFailure describes a rejected HTTP response, not a transport failure or
// interrupted SSE stream. Never retain a provider body, URL or response header.
func HTTPFailure(provider string, status int) *protocol.Error {
	kind := "model_http"
	switch status {
	case 401, 403:
		kind = "model_auth"
	case 429:
		kind = "model_rate_limit"
	case 408, 504:
		kind = "model_timeout"
	case 529:
		if provider == "Anthropic" {
			kind = "model_rate_limit"
		}
	}
	failure := protocol.E(kind, fmt.Sprintf("%s HTTP %d", provider, status))
	failure.Data.Retryable = status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504 || provider == "Anthropic" && status == 529
	return failure
}
