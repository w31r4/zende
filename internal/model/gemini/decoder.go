package gemini

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type outputBlock struct {
	kind, id, name string
	parts          []json.RawMessage
	text           strings.Builder
	args           json.RawMessage
	bytes          int
}

type decoder struct {
	endpoint, model, prefix, responseID string
	emit                                func(model.Delta) error
	blocks                              []*outputBlock
	current                             *outputBlock
	callIDs                             map[string]bool
	finished                            bool
	bytes                               int
	input, candidates, thoughts         *int64
}

func (d *decoder) receive(event string, raw []byte) error {
	if event != "" && event != "message" {
		return protocol.E("unsupported_capability", "Gemini SSE event type is not supported")
	}
	var response struct {
		ResponseID string          `json:"responseId"`
		Error      json.RawMessage `json:"error"`
		Usage      json.RawMessage `json:"usageMetadata"`
		Feedback   struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Candidates []struct {
			Index   int `json:"index"`
			Content struct {
				Role  string            `json:"role"`
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			Finish string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := decodeObject(raw, &response); err != nil {
		return err
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		return protocol.E("model_failed", "Gemini provider reported a stream failure")
	}
	if response.Feedback.BlockReason != "" && response.Feedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
		return protocol.E("model_blocked", "Gemini provider blocked the request")
	}
	if response.ResponseID != "" {
		if len(response.ResponseID) > 512 || d.responseID != "" && d.responseID != response.ResponseID {
			return wireError("response identity changed")
		}
		d.responseID = response.ResponseID
	}
	if err := d.usage(response.Usage); err != nil {
		return err
	}
	if len(response.Candidates) == 0 {
		if len(response.Usage) != 0 && string(response.Usage) != "null" {
			return nil
		}
		return wireError("response has neither candidate nor usage")
	}
	if len(response.Candidates) != 1 || response.Candidates[0].Index != 0 {
		return wireError("expected one response candidate")
	}
	candidate := response.Candidates[0]
	if candidate.Content.Role != "" && candidate.Content.Role != "model" {
		return wireError("candidate role is not model")
	}
	if d.finished && len(candidate.Content.Parts) != 0 {
		return wireError("content follows candidate finish")
	}
	for _, raw := range candidate.Content.Parts {
		if err := d.addPart(raw); err != nil {
			return err
		}
	}
	switch candidate.Finish {
	case "", "FINISH_REASON_UNSPECIFIED":
	case "STOP":
		d.finished = true
	case "MAX_TOKENS", "CONTINUATION":
		return protocol.E("model_incomplete", "Gemini response did not complete within the generation budget")
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
		return wireError("provider rejected the generated function call")
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return protocol.E("model_blocked", "Gemini provider blocked the response")
	default:
		return protocol.E("unsupported_capability", "Gemini finish reason is not supported")
	}
	return nil
}

func (d *decoder) addPart(raw json.RawMessage) error {
	p, err := decodePart(raw)
	if err != nil {
		return err
	}
	kind := "text"
	if p.Thought || p.Text == nil {
		kind = "private"
	}
	if p.Call != nil {
		kind = "call"
	}
	d.bytes += len(raw)
	if d.bytes > model.MaxResponseBytes {
		return protocol.E("output_limit", "Gemini response parts exceed budget")
	}
	b := d.current
	if b == nil || b.kind != kind || kind == "call" {
		if len(d.blocks) >= model.MaxResponseBlocks {
			return protocol.E("output_limit", "too many Gemini response blocks")
		}
		b = &outputBlock{kind: kind, id: fmt.Sprintf("%s_%d", d.prefix, len(d.blocks))}
		d.blocks = append(d.blocks, b)
		d.current = b
		if kind == "call" {
			if d.callIDs == nil {
				d.callIDs = map[string]bool{}
			}
			if p.Call.ID != "" {
				b.id = p.Call.ID
			}
			if d.callIDs[b.id] {
				return wireError("duplicate function call identity")
			}
			d.callIDs[b.id] = true
			b.name, b.args = p.Call.Name, nil
			b.args, err = arguments(p.Call.Args)
			if err != nil {
				return err
			}
			if err := d.emit(model.Delta{Kind: model.CallStart, CallID: b.id, Name: b.name}); err != nil {
				return err
			}
			if err := d.emit(model.Delta{Kind: model.ArgumentsDelta, CallID: b.id, Text: string(b.args)}); err != nil {
				return err
			}
		} else if err := d.emit(model.Delta{Kind: model.MessageStart, MessageID: b.id}); err != nil {
			return err
		}
	}
	b.bytes += len(raw)
	if b.bytes > model.MaxBlockBytes {
		return protocol.E("output_limit", "Gemini saved parts exceed block budget")
	}
	b.parts = append(b.parts, append(json.RawMessage(nil), raw...))
	if kind == "text" && p.Text != nil {
		b.text.WriteString(*p.Text)
		if err := d.emit(model.Delta{Kind: model.TextDelta, MessageID: b.id, Text: *p.Text}); err != nil {
			return err
		}
	}
	return nil
}

// Gemini has no [DONE] sentinel. The adapter calls complete only after a clean
// HTTP/SSE EOF, and STOP must already have appeared. A read error never commits.
func (d *decoder) complete() error {
	if !d.finished {
		return protocol.E("model_incomplete", "Gemini stream ended before candidate finish")
	}
	for _, b := range d.blocks {
		saved := savedParts{Version: 1, Endpoint: d.endpoint, Model: d.model, Parts: b.parts}
		if b.kind == "call" {
			saved.CallID = b.id
		}
		raw, err := json.Marshal(saved)
		if err != nil {
			return wireError("response parts could not be saved")
		}
		if len(raw)+b.text.Len()+len(b.args) > model.MaxBlockBytes {
			return protocol.E("output_limit", "Gemini saved content exceeds block budget")
		}
		data := model.Delta{Kind: model.ProtocolData, Protocol: Protocol, Raw: raw}
		end := model.Delta{Kind: model.MessageEnd}
		if b.kind == "call" {
			data.CallID, end.CallID, end.Kind = b.id, b.id, model.CallEnd
		} else {
			data.MessageID, end.MessageID = b.id, b.id
		}
		if err := d.emit(data); err != nil {
			return err
		}
		if err := d.emit(end); err != nil {
			return err
		}
	}
	return d.emit(model.Delta{Kind: model.ResponseEnd})
}

func (d *decoder) usage(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var usage struct {
		Input      *int64 `json:"promptTokenCount"`
		Candidates *int64 `json:"candidatesTokenCount"`
		Thoughts   *int64 `json:"thoughtsTokenCount"`
		Cached     *int64 `json:"cachedContentTokenCount"`
		Total      *int64 `json:"totalTokenCount"`
	}
	if err := decodeObject(raw, &usage); err != nil {
		return err
	}
	for _, value := range []*int64{usage.Input, usage.Candidates, usage.Thoughts, usage.Cached, usage.Total} {
		if value != nil && *value < 0 {
			return wireError("negative token usage")
		}
	}
	if usage.Input != nil {
		d.input = usage.Input
	}
	if usage.Candidates != nil {
		d.candidates = usage.Candidates
	}
	if usage.Thoughts != nil {
		d.thoughts = usage.Thoughts
	}
	result := &model.Usage{InputTokens: d.input}
	if d.candidates != nil {
		total := *d.candidates
		if d.thoughts != nil {
			if *d.thoughts > math.MaxInt64-total {
				return wireError("token usage exceeds numeric range")
			}
			total += *d.thoughts
		}
		result.OutputTokens = &total
	}
	return d.emit(model.Delta{Kind: model.UsageUpdate, Usage: result})
}
