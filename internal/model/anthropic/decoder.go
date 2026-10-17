package anthropic

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type blockState struct {
	kind, id, name             string
	fields                     map[string]json.RawMessage
	text, arguments, signature strings.Builder
	initialInput               json.RawMessage
	inputDelta, closed         bool
	bytes                      int
}

type decoder struct {
	endpoint, model, id, finish          string
	emit                                 func(model.Delta) error
	blocks                               []*blockState
	callIDs                              map[string]bool
	clientSearch, searchAdvertised       bool
	bytes, calls                         int
	input, output, cacheRead, cacheWrite *int64
}

func (d *decoder) budget(block *blockState, size int) error {
	d.bytes += size
	if block != nil {
		block.bytes += size
		if block.bytes > model.MaxBlockBytes {
			return protocol.E("output_limit", "Anthropic block exceeds budget")
		}
	}
	if d.bytes > model.MaxResponseBytes {
		return protocol.E("output_limit", "Anthropic response exceeds budget")
	}
	return nil
}

func (d *decoder) receive(name string, raw []byte) (bool, error) {
	var event struct {
		Type         string          `json:"type"`
		Index        *int            `json:"index"`
		Message      json.RawMessage `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
		Usage        json.RawMessage `json:"usage"`
	}
	if err := decodeObject(raw, &event); err != nil {
		return false, err
	}
	if event.Type == "" || name != "" && name != event.Type {
		return false, wireError("SSE event name does not match its type")
	}
	if event.Type == "ping" {
		return false, nil
	}
	if event.Type == "error" {
		return false, protocol.E("model_failed", "Anthropic provider reported a stream failure")
	}
	if event.Type == "message_start" {
		var message struct {
			ID, Type, Role string
			Content        []json.RawMessage `json:"content"`
			StopReason     *string           `json:"stop_reason"`
			Usage          json.RawMessage   `json:"usage"`
		}
		if err := decodeObject(event.Message, &message); err != nil {
			return false, err
		}
		if d.id != "" || message.ID == "" || len(message.ID) > 512 || message.Type != "message" || message.Role != "assistant" || len(message.Content) != 0 || message.StopReason != nil {
			return false, wireError("invalid message start")
		}
		d.id, d.callIDs = message.ID, map[string]bool{}
		return false, d.usage(message.Usage)
	}
	if d.id == "" {
		return false, wireError("event precedes message start")
	}
	switch event.Type {
	case "content_block_start":
		if d.finish != "" || event.Index == nil || *event.Index != len(d.blocks) || len(d.blocks) >= model.MaxResponseBlocks {
			return false, wireError("invalid content block index or ordering")
		}
		return false, d.startBlock(*event.Index, event.ContentBlock)
	case "content_block_delta", "content_block_stop":
		if d.finish != "" || event.Index == nil || *event.Index < 0 || *event.Index >= len(d.blocks) || d.blocks[*event.Index].closed {
			return false, wireError("content block is missing or already closed")
		}
		block := d.blocks[*event.Index]
		if event.Type == "content_block_stop" {
			if err := d.closeBlock(block); err != nil {
				return false, err
			}
			block.closed = true
			return false, nil
		}
		return false, d.delta(block, event.Delta)
	case "message_delta":
		for _, block := range d.blocks {
			if !block.closed {
				return false, wireError("message delta precedes a content block stop")
			}
		}
		var delta struct {
			StopReason   *string `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		}
		if err := decodeObject(event.Delta, &delta); err != nil {
			return false, err
		}
		if delta.StopSequence != nil {
			return false, wireError("received an unrequested stop sequence")
		}
		if delta.StopReason != nil {
			if d.finish != "" && d.finish != *delta.StopReason {
				return false, wireError("stop reason changed")
			}
			switch *delta.StopReason {
			case "end_turn", "refusal":
				if d.calls != 0 {
					return false, wireError("stop reason conflicts with tool calls")
				}
			case "tool_use":
				if d.calls == 0 {
					return false, wireError("tool stop reason has no calls")
				}
			case "max_tokens", "model_context_window_exceeded":
				return false, protocol.E("model_incomplete", "Anthropic response exceeded its output or context budget")
			default:
				return false, protocol.E("unsupported_capability", "Anthropic stop reason is not supported")
			}
			d.finish = *delta.StopReason
		}
		return false, d.usage(event.Usage)
	case "message_stop":
		return d.complete()
	default:
		return false, protocol.E("unsupported_capability", "Anthropic stream event type is not supported")
	}
}

func (d *decoder) startBlock(index int, raw []byte) error {
	var value wireBlock
	if err := decodeObject(raw, &value); err != nil {
		return err
	}
	b := &blockState{kind: value.Type, id: fmt.Sprintf("%s:%d", d.id, index)}
	if err := json.Unmarshal(raw, &b.fields); err != nil {
		return wireError("invalid content block")
	}
	if err := d.budget(b, len(raw)); err != nil {
		return err
	}
	switch value.Type {
	case "text":
		if len(value.Citations) != 0 && string(value.Citations) != "null" && !sameJSON(value.Citations, []byte("[]")) {
			return protocol.E("unsupported_capability", "Anthropic citations are not enabled")
		}
		b.text.WriteString(value.Text)
	case "thinking":
		b.text.WriteString(value.Thinking)
		b.signature.WriteString(value.Signature)
	case "redacted_thinking":
		if value.Data == "" {
			return wireError("redacted thinking data is missing")
		}
	case "tool_use":
		var input map[string]json.RawMessage
		if !validCall(value.ID, value.Name) || d.callIDs[value.ID] || decodeObject(value.Input, &input) != nil {
			return wireError("invalid or duplicate tool call")
		}
		if d.clientSearch && value.Name == searchToolName && !d.searchAdvertised {
			return wireError("native search was not offered for this request")
		}
		b.id, b.name, b.initialInput = value.ID, value.Name, value.Input
		d.callIDs[value.ID] = true
		d.calls++
	default:
		return protocol.E("unsupported_capability", "Anthropic content block type is not supported")
	}
	d.blocks = append(d.blocks, b)
	if b.kind == "tool_use" {
		return d.emit(model.Delta{Kind: model.CallStart, CallID: b.id, Name: b.name})
	}
	if err := d.emit(model.Delta{Kind: model.MessageStart, MessageID: b.id}); err != nil {
		return err
	}
	if b.kind == "text" && b.text.Len() != 0 {
		return d.emit(model.Delta{Kind: model.TextDelta, MessageID: b.id, Text: b.text.String()})
	}
	return nil
}

func (d *decoder) delta(b *blockState, raw []byte) error {
	var delta struct {
		Type        string  `json:"type"`
		Text        *string `json:"text,omitempty"`
		Thinking    *string `json:"thinking,omitempty"`
		Signature   *string `json:"signature,omitempty"`
		PartialJSON *string `json:"partial_json,omitempty"`
	}
	if protocol.StrictDecode(raw, &delta) != nil {
		return wireError("invalid content delta")
	}
	var text *string
	switch {
	case b.kind == "text" && delta.Type == "text_delta":
		text = delta.Text
	case b.kind == "thinking" && delta.Type == "thinking_delta":
		text = delta.Thinking
	case b.kind == "thinking" && delta.Type == "signature_delta":
		text = delta.Signature
	case b.kind == "tool_use" && delta.Type == "input_json_delta":
		if !sameJSON(b.initialInput, []byte("{}")) {
			return wireError("partial arguments conflict with initial tool input")
		}
		text = delta.PartialJSON
	default:
		return protocol.E("unsupported_capability", "Anthropic content delta does not match its block")
	}
	if text == nil {
		return wireError("content delta is missing its value")
	}
	if err := d.budget(b, len(*text)); err != nil {
		return err
	}
	switch delta.Type {
	case "text_delta":
		b.text.WriteString(*text)
		return d.emit(model.Delta{Kind: model.TextDelta, MessageID: b.id, Text: *text})
	case "thinking_delta":
		b.text.WriteString(*text)
	case "signature_delta":
		b.signature.WriteString(*text)
	case "input_json_delta":
		b.inputDelta = true
		b.arguments.WriteString(*text)
		return d.emit(model.Delta{Kind: model.ArgumentsDelta, CallID: b.id, Text: *text})
	}
	return nil
}

func (d *decoder) closeBlock(b *blockState) error {
	set := func(key string, value any) { b.fields[key], _ = json.Marshal(value) }
	switch b.kind {
	case "text":
		set("text", b.text.String())
	case "thinking":
		if b.signature.Len() == 0 {
			return wireError("thinking signature is missing")
		}
		set("thinking", b.text.String())
		set("signature", b.signature.String())
	case "tool_use":
		if !b.inputDelta {
			b.arguments.Write(b.initialInput)
			if err := d.emit(model.Delta{Kind: model.ArgumentsDelta, CallID: b.id, Text: b.arguments.String()}); err != nil {
				return err
			}
		}
		var input map[string]json.RawMessage
		if decodeObject([]byte(b.arguments.String()), &input) != nil {
			return wireError("tool arguments are not a complete JSON object")
		}
		b.fields["input"] = json.RawMessage(b.arguments.String())
	}
	return nil
}

func (d *decoder) complete() (bool, error) {
	if d.finish == "" {
		return false, protocol.E("model_incomplete", "Anthropic message ended without a stop reason")
	}
	for _, block := range d.blocks {
		if !block.closed {
			return false, wireError("message ended with an open content block")
		}
	}
	for _, b := range d.blocks {
		raw, err := json.Marshal(b.fields)
		if err != nil {
			return false, wireError("content block could not be saved")
		}
		version := 1
		if d.clientSearch && d.searchAdvertised && b.kind == "tool_use" && b.name == searchToolName {
			version = 2
		}
		saved, err := json.Marshal(savedBlock{Version: version, Endpoint: d.endpoint, Model: d.model, Block: raw})
		if err != nil {
			return false, wireError("content block could not be saved")
		}
		publicSize := 0
		if b.kind == "text" {
			publicSize = b.text.Len()
		} else if b.kind == "tool_use" {
			publicSize = b.arguments.Len()
		}
		if len(saved)+publicSize > model.MaxBlockBytes {
			return false, protocol.E("output_limit", "Anthropic saved block exceeds budget")
		}
		data := model.Delta{Kind: model.ProtocolData, Protocol: Protocol, Raw: saved}
		end := model.Delta{Kind: model.MessageEnd}
		if b.kind == "tool_use" {
			data.CallID, end.CallID, end.Kind = b.id, b.id, model.CallEnd
		} else {
			data.MessageID, end.MessageID = b.id, b.id
		}
		if err := d.emit(data); err != nil {
			return false, err
		}
		if err := d.emit(end); err != nil {
			return false, err
		}
	}
	if err := d.emit(model.Delta{Kind: model.ResponseEnd}); err != nil {
		return false, err
	}
	return true, nil
}

func (d *decoder) usage(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var usage struct {
		Input      *int64 `json:"input_tokens"`
		Output     *int64 `json:"output_tokens"`
		CacheRead  *int64 `json:"cache_read_input_tokens"`
		CacheWrite *int64 `json:"cache_creation_input_tokens"`
	}
	if err := decodeObject(raw, &usage); err != nil {
		return err
	}
	for _, pair := range []struct {
		value  *int64
		target **int64
	}{
		{usage.Input, &d.input}, {usage.Output, &d.output}, {usage.CacheRead, &d.cacheRead}, {usage.CacheWrite, &d.cacheWrite},
	} {
		if pair.value != nil {
			if *pair.value < 0 {
				return wireError("negative token usage")
			}
			*pair.target = pair.value
		}
	}
	result := &model.Usage{OutputTokens: d.output}
	if d.input != nil {
		total := *d.input
		for _, count := range []*int64{d.cacheRead, d.cacheWrite} {
			if count != nil {
				if *count > math.MaxInt64-total {
					return wireError("token usage exceeds numeric range")
				}
				total += *count
			}
		}
		result.InputTokens = &total
	}
	return d.emit(model.Delta{Kind: model.UsageUpdate, Usage: result})
}
