package model

import (
	"encoding/json"
	"math"
	"reflect"

	"zende.local/harness/internal/protocol"
)

// This heuristic intentionally includes structured metadata, opaque replay
// bytes and fixed image allowances. It is not a tokenizer or a fit guarantee.
const ContextEstimateMethod = "json-bytes/4+structure+1200-per-image-v1"

func tokenBytes(n int) int64 {
	return int64(n/4) + int64((n%4+3)/4)
}

func estimateJSON(value any) (int64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, protocol.E("context_invalid", "context cannot be encoded for capacity estimation")
	}
	return tokenBytes(len(data)), nil
}

func estimateMessage(message Message) (protocol.ContextTokenEstimate, error) {
	private := message.Opaque
	message.Opaque = nil
	count, err := estimateJSON(message)
	if err != nil {
		return protocol.ContextTokenEstimate{}, err
	}
	out := protocol.ContextTokenEstimate{Method: ContextEstimateMethod, Messages: count + 8, Private: tokenBytes(len(private)), MessageCount: 1}
	for _, b := range message.Content {
		if b.Type == "image" {
			out.Images += 1200
			out.ImageCount++
		}
	}
	out.InputTokens = out.Messages + out.Private + out.Images
	return out, nil
}

// EstimateMessageTokens is also used when choosing a complete retained tail.
func EstimateMessageTokens(message Message) (int64, error) {
	out, err := estimateMessage(message)
	return out.InputTokens, err
}

func EstimateRequest(r Request) (protocol.ContextTokenEstimate, error) {
	out := protocol.ContextTokenEstimate{Method: ContextEstimateMethod, System: tokenBytes(len(r.System)), Overhead: 16}
	for _, message := range r.Messages {
		count, err := estimateMessage(message)
		if err != nil {
			return out, err
		}
		out.Messages += count.Messages
		out.Private += count.Private
		out.Images += count.Images
		out.MessageCount++
		out.ImageCount += count.ImageCount
	}
	for _, tool := range r.Tools {
		count, err := estimateJSON(tool)
		if err != nil {
			return out, err
		}
		out.Tools += count + 16
	}
	total, ok := estimateTotal(out)
	if !ok {
		return out, protocol.E("context_limit", "context estimate exceeds the accounting range")
	}
	out.InputTokens = total
	return out, nil
}

func estimateTotal(e protocol.ContextTokenEstimate) (int64, bool) {
	if e.Method != ContextEstimateMethod || e.MessageCount < 0 || e.ImageCount < 0 {
		return 0, false
	}
	var total int64
	for _, n := range []int64{e.System, e.Messages, e.Tools, e.Private, e.Images, e.Overhead} {
		if n < 0 || n > math.MaxInt64-total {
			return 0, false
		}
		total += n
	}
	return total, true
}

// PlanContextBudget is pure: it does not send a request, compact history or
// resolve credentials. Normal and summary outputs are alternative requests;
// reserve the larger output plus headroom before applying the trigger ratio.
func PlanContextBudget(e protocol.ContextTokenEstimate, window, output int64, p protocol.ContextBudgetPolicy) (protocol.ContextBudget, error) {
	out := protocol.ContextBudget{Policy: p, Estimate: e, ContextWindow: window, OutputReserve: output, State: "capacity_unknown"}
	total, ok := estimateTotal(e)
	if !ok || total != e.InputTokens || !p.Valid() || window < 0 || output < 0 {
		return out, protocol.E("context_invalid", "invalid context estimate, capacity or budget policy")
	}
	if window == 0 || output == 0 {
		return out, nil
	}
	out.SummaryOutputReserve = min(int64(p.SummaryTokens), output)
	hard := window - output
	headroom := int64(0)
	if hard > int64(p.ReserveTokens) {
		headroom = hard - int64(p.ReserveTokens)
	}
	// Divide before multiplying so even a very large declared window is safe.
	trigger := int64(0)
	if headroom > 0 {
		trigger = headroom/100*int64(p.TriggerPercent) + headroom%100*int64(p.TriggerPercent)/100
	}
	out.InputLimit, out.TriggerAt = &hard, &trigger
	switch {
	case hard <= 0 || headroom <= 0:
		out.State = "insufficient_capacity"
	case e.InputTokens > hard:
		out.State = "over_capacity"
	case e.InputTokens >= trigger:
		out.State = "compaction_needed"
	default:
		out.State = "within_budget"
	}
	return out, nil
}

func ValidContextBudget(b protocol.ContextBudget) bool {
	expected, err := PlanContextBudget(b.Estimate, b.ContextWindow, b.OutputReserve, b.Policy)
	return err == nil && reflect.DeepEqual(b, expected)
}
