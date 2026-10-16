package chatcompletions

import (
	"bytes"
	"encoding/json"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type callState struct {
	id, name  string
	arguments strings.Builder
	started   bool
}

type decoder struct {
	endpoint, model, reasoningField, id, finish string
	emit                                        func(model.Delta) error
	text, content, refusal, reasoning           strings.Builder
	messageStarted                              bool
	calls                                       map[int]*callState
	ids                                         map[string]bool
	bytes                                       int
}

func (d *decoder) budget(n int) error {
	d.bytes += n
	if d.bytes > model.MaxResponseBytes {
		return protocol.E("output_limit", "Chat Completions assembled content exceeds budget")
	}
	return nil
}

func (d *decoder) startText() error {
	if d.messageStarted {
		return nil
	}
	d.messageStarted = true
	return d.emit(model.Delta{Kind: model.MessageStart, MessageID: d.id + ":text"})
}

func (d *decoder) receive(name string, raw []byte) (bool, error) {
	if name != "" && name != "message" {
		return false, protocol.E("unsupported_capability", "Chat Completions SSE event type is not supported")
	}
	if bytes.Equal(raw, []byte("[DONE]")) {
		return d.complete()
	}
	var chunk struct {
		ID      string          `json:"id"`
		Object  string          `json:"object"`
		Error   json.RawMessage `json:"error"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index  *int            `json:"index"`
			Delta  json.RawMessage `json:"delta"`
			Finish *string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := decodeObject(raw, &chunk); err != nil {
		return false, err
	}
	if len(chunk.Error) != 0 && string(chunk.Error) != "null" {
		return false, protocol.E("model_failed", "Chat Completions provider reported failure")
	}
	if chunk.ID == "" || len(chunk.ID) > 512 || d.id != "" && chunk.ID != d.id || chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
		return false, wireError("missing or changed response identity")
	}
	d.id = chunk.ID
	if len(chunk.Usage) != 0 && string(chunk.Usage) != "null" {
		var usage struct {
			Input  *int64 `json:"prompt_tokens"`
			Output *int64 `json:"completion_tokens"`
		}
		if err := decodeObject(chunk.Usage, &usage); err != nil {
			return false, err
		}
		if usage.Input != nil && *usage.Input < 0 || usage.Output != nil && *usage.Output < 0 {
			return false, wireError("negative usage")
		}
		if err := d.emit(model.Delta{Kind: model.UsageUpdate, Usage: &model.Usage{InputTokens: usage.Input, OutputTokens: usage.Output}}); err != nil {
			return false, err
		}
	}
	if len(chunk.Choices) == 0 {
		if len(chunk.Usage) == 0 || string(chunk.Usage) == "null" {
			return false, wireError("chunk has neither choice nor usage")
		}
		return false, nil
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Index == nil || *chunk.Choices[0].Index != 0 || d.finish != "" {
		return false, wireError("unexpected choice or content after finish")
	}
	choice := chunk.Choices[0]
	var delta map[string]json.RawMessage
	if err := decodeObject(choice.Delta, &delta); err != nil {
		return false, err
	}
	for key, value := range delta {
		if string(value) == "null" {
			continue
		}
		switch key {
		case "role", "content", "refusal", "tool_calls":
		default:
			if key != d.reasoningField || key == "" {
				return false, protocol.E("unsupported_capability", "Chat Completions delta has an undeclared content field")
			}
		}
	}
	var role string
	if value, ok := delta["role"]; ok && string(value) != "null" {
		if json.Unmarshal(value, &role) != nil || role != "assistant" {
			return false, wireError("output role is not assistant")
		}
	}
	for _, field := range []string{"content", "refusal", d.reasoningField} {
		if field == "" {
			continue
		}
		value, ok := delta[field]
		if !ok || string(value) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return false, wireError("content delta is not text")
		}
		if err := d.budget(len(text)); err != nil {
			return false, err
		}
		if field == d.reasoningField {
			if d.reasoning.Len()+len(text) > model.MaxBlockBytes {
				return false, protocol.E("output_limit", "Chat Completions private content exceeds budget")
			}
			d.reasoning.WriteString(text)
			if err := d.startText(); err != nil {
				return false, err
			}
			continue
		}
		if text == "" {
			continue
		}
		if d.text.Len()+len(text) > model.MaxBlockBytes {
			return false, protocol.E("output_limit", "Chat Completions text exceeds budget")
		}
		if field == "content" {
			if d.refusal.Len() != 0 {
				return false, wireError("content follows refusal")
			}
			d.content.WriteString(text)
		} else {
			d.refusal.WriteString(text)
		}
		d.text.WriteString(text)
		if err := d.startText(); err != nil {
			return false, err
		}
		if err := d.emit(model.Delta{Kind: model.TextDelta, MessageID: d.id + ":text", Text: text}); err != nil {
			return false, err
		}
	}
	if value, ok := delta["tool_calls"]; ok && string(value) != "null" {
		var calls []struct {
			Index    *int   `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string  `json:"name"`
				Arguments *string `json:"arguments"`
			} `json:"function"`
		}
		if protocol.StrictDecode(value, &calls) != nil {
			return false, wireError("invalid function call delta")
		}
		for _, call := range calls {
			if call.Index == nil || *call.Index < 0 || *call.Index >= model.MaxResponseBlocks-1 || call.Type != "" && call.Type != "function" {
				return false, wireError("invalid function call index or type")
			}
			if d.calls == nil {
				d.calls = map[int]*callState{}
				d.ids = map[string]bool{}
			}
			state := d.calls[*call.Index]
			if state == nil {
				state = &callState{}
				d.calls[*call.Index] = state
			}
			if call.ID != "" {
				if len(call.ID) > 512 || state.id != "" && state.id != call.ID || state.id == "" && d.ids[call.ID] {
					return false, wireError("duplicate or changed function call identity")
				}
				state.id = call.ID
				d.ids[call.ID] = true
			}
			if call.Function.Name != "" {
				if !functionName.MatchString(call.Function.Name) || state.name != "" && state.name != call.Function.Name {
					return false, wireError("invalid or changed function name")
				}
				state.name = call.Function.Name
			}
			if call.Function.Arguments != nil {
				text := *call.Function.Arguments
				if err := d.budget(len(text)); err != nil {
					return false, err
				}
				if state.arguments.Len()+len(text) > model.MaxBlockBytes {
					return false, protocol.E("output_limit", "Chat Completions arguments exceed budget")
				}
				state.arguments.WriteString(text)
				if state.started {
					if err := d.emit(model.Delta{Kind: model.ArgumentsDelta, CallID: state.id, Text: text}); err != nil {
						return false, err
					}
				}
			}
			if !state.started && state.id != "" && state.name != "" {
				state.started = true
				if err := d.emit(model.Delta{Kind: model.CallStart, CallID: state.id, Name: state.name}); err != nil {
					return false, err
				}
				if err := d.emit(model.Delta{Kind: model.ArgumentsDelta, CallID: state.id, Text: state.arguments.String()}); err != nil {
					return false, err
				}
			}
		}
	}
	if choice.Finish != nil {
		switch *choice.Finish {
		case "stop":
			if len(d.calls) != 0 {
				return false, wireError("stop conflicts with function calls")
			}
		case "tool_calls":
			if len(d.calls) == 0 {
				return false, wireError("tool finish has no calls")
			}
		case "length", "content_filter":
			return false, protocol.E("model_incomplete", "Chat Completions response was truncated or filtered")
		default:
			return false, protocol.E("unsupported_capability", "Chat Completions finish reason is not supported")
		}
		d.finish = *choice.Finish
	}
	return false, nil
}

func (d *decoder) complete() (bool, error) {
	if d.finish == "" {
		return false, protocol.E("model_incomplete", "Chat Completions sentinel arrived before finish")
	}
	// Validate all calls before emitting any completion; no partial arguments can
	// authorize a tool even if a later call in the same response is malformed.
	for i := 0; i < len(d.calls); i++ {
		call := d.calls[i]
		var args map[string]json.RawMessage
		if call == nil || !call.started || decodeObject([]byte(call.arguments.String()), &args) != nil {
			return false, wireError("incomplete function call")
		}
	}
	if d.messageStarted {
		if d.reasoning.Len() != 0 || d.refusal.Len() != 0 {
			raw, _ := json.Marshal(privateMessage{Version: 1, Endpoint: d.endpoint, Model: d.model, TextHash: textHash(d.text.String()), Content: d.content.String(), Refusal: d.refusal.String(), ReasoningField: d.reasoningField, Reasoning: d.reasoning.String()})
			if len(raw)+d.text.Len() > model.MaxBlockBytes {
				return false, protocol.E("output_limit", "Chat Completions saved message exceeds budget")
			}
			if err := d.emit(model.Delta{Kind: model.ProtocolData, MessageID: d.id + ":text", Protocol: Protocol, Raw: raw}); err != nil {
				return false, err
			}
		}
		if err := d.emit(model.Delta{Kind: model.MessageEnd, MessageID: d.id + ":text"}); err != nil {
			return false, err
		}
	}
	for i := 0; i < len(d.calls); i++ {
		if err := d.emit(model.Delta{Kind: model.CallEnd, CallID: d.calls[i].id}); err != nil {
			return false, err
		}
	}
	if err := d.emit(model.Delta{Kind: model.ResponseEnd}); err != nil {
		return false, err
	}
	return true, nil
}
