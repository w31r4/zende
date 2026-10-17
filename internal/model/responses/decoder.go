package responses

import (
	"bytes"
	"encoding/json"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

type event struct {
	Type         string          `json:"type"`
	Sequence     *int64          `json:"sequence_number"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	ItemID       string          `json:"item_id"`
	Item         json.RawMessage `json:"item"`
	Part         json.RawMessage `json:"part"`
	Delta        *string         `json:"delta"`
	Text         *string         `json:"text"`
	Refusal      *string         `json:"refusal"`
	Arguments    *string         `json:"arguments"`
	Response     json.RawMessage `json:"response"`
}

type partState struct {
	kind string
	text strings.Builder
	done bool
}
type itemState struct {
	item          wireItem
	parts         map[int]*partState
	text          strings.Builder
	arguments     strings.Builder
	argumentsDone bool
	done          bool
	raw           json.RawMessage
}
type decoder struct {
	endpoint, model, responseID string
	emit                        func(model.Delta) error
	items                       []*itemState
	ids                         map[string]bool
	calls                       map[string]bool
	sequence                    *int64
	bytes                       int
	clientSearch                bool
	searchAdvertised            bool
}

func (d *decoder) send(delta model.Delta) error { return d.emit(delta) }

func (d *decoder) receive(name string, raw []byte) (bool, error) {
	var e event
	if err := decodeObject(raw, &e); err != nil {
		return false, err
	}
	if e.Type == "" || (name != "" && name != e.Type) {
		return false, wireError("SSE event type mismatch")
	}
	if e.Sequence != nil {
		if *e.Sequence < 0 || d.sequence != nil && *e.Sequence <= *d.sequence {
			return false, wireError("non-increasing event sequence")
		}
		d.sequence = e.Sequence
	}
	switch e.Type {
	case "response.created", "response.in_progress", "response.queued":
		var r struct {
			ID string `json:"id"`
		}
		if err := decodeObject(e.Response, &r); err != nil {
			return false, err
		}
		return false, d.identify(r.ID)
	case "response.output_item.added":
		return false, d.start(e)
	case "response.output_item.done":
		return false, d.finish(e)
	case "response.output_text.delta", "response.refusal.delta", "response.output_text.done", "response.refusal.done", "response.content_part.added", "response.content_part.done":
		return false, d.content(e)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		s, err := d.lookup(e, true)
		if err != nil {
			return false, err
		}
		if s.item.Type != "function_call" || s.argumentsDone {
			return false, wireError("arguments outside an open function call")
		}
		if e.Type == "response.function_call_arguments.done" {
			if e.Arguments == nil {
				return false, wireError("missing final arguments")
			}
			if err := d.arguments(s, *e.Arguments, true); err != nil {
				return false, err
			}
			s.argumentsDone = true
			return false, nil
		}
		if e.Delta == nil {
			return false, wireError("missing arguments delta")
		}
		return false, d.arguments(s, *e.Delta, false)
	case "response.completed":
		return d.complete(e.Response)
	case "response.failed", "error":
		return false, protocol.E("model_failed", "Responses provider reported failure")
	case "response.incomplete":
		return false, protocol.E("model_incomplete", "Responses provider reported an incomplete response")
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		// These are private reasoning, not visible commentary. The complete raw
		// reasoning item is saved by output_item.done, including encrypted state.
		s, err := d.lookup(e, true)
		if err != nil {
			return false, err
		}
		if s.item.Type != "reasoning" {
			return false, wireError("reasoning event targets another item type")
		}
		return false, nil
	case "response.output_text.annotation.added":
		s, err := d.lookup(e, true)
		if err != nil {
			return false, err
		}
		if s.item.Type != "message" {
			return false, wireError("annotation targets another item type")
		}
		return false, nil // Preserved in the final raw content part.
	default:
		// Reject unknown semantics rather than silently dropping calls or output.
		return false, protocol.E("unsupported_capability", "Responses stream event is not supported")
	}
}

func (d *decoder) identify(id string) error {
	if id == "" || len(id) > 512 || d.responseID != "" && d.responseID != id {
		return wireError("missing or changed response identity")
	}
	d.responseID = id
	return nil
}

func (d *decoder) start(e event) error {
	if e.OutputIndex == nil || *e.OutputIndex != len(d.items) || len(d.items) >= model.MaxResponseBlocks {
		return wireError("invalid output item order or count")
	}
	var item wireItem
	if err := decodeObject(e.Item, &item); err != nil {
		return err
	}
	if (item.ID == "" && item.Type != "tool_search_call") || len(item.ID) > 512 {
		return wireError("invalid item identity")
	}
	if d.ids == nil {
		d.ids = map[string]bool{}
		d.calls = map[string]bool{}
	}
	if item.ID != "" && d.ids[item.ID] {
		return wireError("duplicate item identity")
	}
	if item.ID != "" {
		d.ids[item.ID] = true
	}
	s := &itemState{item: item, parts: map[int]*partState{}}
	d.items = append(d.items, s)
	switch item.Type {
	case "message":
		if item.Role != "assistant" {
			return wireError("output message is not assistant")
		}
		cat, err := category(item.Phase)
		if err != nil {
			return err
		}
		if err := d.send(model.Delta{Kind: model.MessageStart, MessageID: item.ID, Category: cat}); err != nil {
			return err
		}
		for i, p := range item.Content {
			if err := d.setPart(s, i, p, false); err != nil {
				return err
			}
		}
		return d.flush(s)
	case "reasoning":
		return d.send(model.Delta{Kind: model.MessageStart, MessageID: item.ID})
	case "function_call", "tool_search_call":
		name, arguments, err := item.call(d.clientSearch)
		if err != nil {
			return err
		}
		if item.Type == "tool_search_call" && !d.searchAdvertised {
			return wireError("native search was not offered for this request")
		}
		if item.Type == "function_call" && name == searchToolName && d.searchAdvertised {
			return wireError("native search returned an ordinary function call")
		}
		if item.CallID == "" || len(item.CallID) > 512 || name == "" || len(name) > 256 || d.calls[item.CallID] {
			return wireError("invalid function call identity")
		}
		d.calls[item.CallID] = true
		if err := d.send(model.Delta{Kind: model.CallStart, CallID: item.CallID, Name: name}); err != nil {
			return err
		}
		if item.Type == "tool_search_call" {
			// Search arguments are object-valued, not streamed JSON strings.
			// Publish them only from the complete output_item.done object.
			return nil
		}
		return d.arguments(s, arguments, false)
	default:
		return protocol.E("unsupported_capability", "Responses output item type is not supported")
	}
}

func (d *decoder) lookup(e event, needsID bool) (*itemState, error) {
	if e.OutputIndex == nil || *e.OutputIndex < 0 || *e.OutputIndex >= len(d.items) {
		return nil, wireError("event targets an unknown item")
	}
	s := d.items[*e.OutputIndex]
	if s.done || needsID && e.ItemID != s.item.ID {
		return nil, wireError("event targets a closed or mismatched item")
	}
	return s, nil
}

func (d *decoder) budget(n int) error {
	d.bytes += n
	if d.bytes > model.MaxResponseBytes {
		return protocol.E("output_limit", "Responses assembled content exceeds budget")
	}
	return nil
}

func (d *decoder) arguments(s *itemState, text string, final bool) error {
	if final {
		if !strings.HasPrefix(text, s.arguments.String()) || s.argumentsDone && text != s.arguments.String() {
			return wireError("final arguments disagree with streamed arguments")
		}
		text = text[s.arguments.Len():]
	}
	if s.arguments.Len()+len(text) > model.MaxBlockBytes {
		return protocol.E("output_limit", "Responses arguments exceed budget")
	}
	if err := d.budget(len(text)); err != nil {
		return err
	}
	s.arguments.WriteString(text)
	if text != "" {
		return d.send(model.Delta{Kind: model.ArgumentsDelta, CallID: s.item.CallID, Text: text})
	}
	return nil
}

func (d *decoder) setPart(s *itemState, index int, part wirePart, done bool) error {
	if index < 0 || index >= model.MaxResponseBlocks {
		return wireError("invalid content index")
	}
	text := part.Text
	if part.Type == "refusal" {
		text = part.Refusal
	} else if part.Type != "output_text" {
		return protocol.E("unsupported_capability", "Responses content part is not supported")
	}
	p := s.parts[index]
	if p == nil {
		p = &partState{kind: part.Type}
		s.parts[index] = p
	}
	if p.kind != part.Type || !strings.HasPrefix(text, p.text.String()) || p.done && text != p.text.String() {
		return wireError("final content disagrees with streamed content")
	}
	if len(text) > model.MaxBlockBytes {
		return protocol.E("output_limit", "Responses content part exceeds budget")
	}
	if err := d.budget(len(text) - p.text.Len()); err != nil {
		return err
	}
	p.text.WriteString(text[p.text.Len():])
	p.done = p.done || done
	return nil
}

func (d *decoder) flush(s *itemState) error {
	var text strings.Builder
	for i := 0; i < len(s.parts); i++ {
		p := s.parts[i]
		if p == nil {
			break
		}
		text.WriteString(p.text.String())
		if !p.done {
			break
		}
	}
	full := text.String()
	if !strings.HasPrefix(full, s.text.String()) {
		return wireError("content order changed")
	}
	if len(full) > model.MaxBlockBytes {
		return protocol.E("output_limit", "Responses message exceeds budget")
	}
	rest := full[s.text.Len():]
	s.text.WriteString(rest)
	if rest != "" {
		return d.send(model.Delta{Kind: model.TextDelta, MessageID: s.item.ID, Text: rest})
	}
	return nil
}

func (d *decoder) content(e event) error {
	s, err := d.lookup(e, true)
	if err != nil {
		return err
	}
	if s.item.Type != "message" || e.ContentIndex == nil || *e.ContentIndex < 0 || *e.ContentIndex >= model.MaxResponseBlocks {
		return wireError("content outside an open message")
	}
	i := *e.ContentIndex
	if strings.HasPrefix(e.Type, "response.content_part.") {
		var p wirePart
		if err := decodeObject(e.Part, &p); err != nil {
			return err
		}
		if err := d.setPart(s, i, p, e.Type == "response.content_part.done"); err != nil {
			return err
		}
	} else {
		kind := "output_text"
		if strings.HasPrefix(e.Type, "response.refusal.") {
			kind = "refusal"
		}
		p := s.parts[i]
		if p == nil {
			p = &partState{kind: kind}
			s.parts[i] = p
		}
		if p.kind != kind || p.done {
			return wireError("content delta targets a completed or mismatched part")
		}
		if strings.HasSuffix(e.Type, ".done") {
			text := e.Text
			if kind == "refusal" {
				text = e.Refusal
			}
			if text == nil {
				return wireError("missing final content")
			}
			if err := d.setPart(s, i, wirePart{Type: kind, Text: *text, Refusal: *text}, true); err != nil {
				return err
			}
		} else {
			if e.Delta == nil {
				return wireError("missing text delta")
			}
			if p.text.Len()+len(*e.Delta) > model.MaxBlockBytes {
				return protocol.E("output_limit", "Responses content part exceeds budget")
			}
			if err := d.budget(len(*e.Delta)); err != nil {
				return err
			}
			p.text.WriteString(*e.Delta)
		}
	}
	return d.flush(s)
}

func (d *decoder) finish(e event) error {
	s, err := d.lookup(e, false)
	if err != nil {
		return err
	}
	var item wireItem
	if err := decodeObject(e.Item, &item); err != nil {
		return err
	}
	if item.ID != s.item.ID || item.Type != s.item.Type || item.Status != "" && item.Status != "completed" {
		return wireError("incomplete or mismatched final item")
	}
	switch item.Type {
	case "message":
		if item.Role != "assistant" || len(item.Content) < len(s.parts) {
			return wireError("final message lost content")
		}
		for index := range s.parts {
			if index >= len(item.Content) {
				return wireError("final message lost a streamed content part")
			}
		}
		cat, err := category(item.Phase)
		if err != nil {
			return err
		}
		if s.item.Phase != "" && s.item.Phase != item.Phase {
			return wireError("assistant phase changed")
		}
		if cat != "" && s.item.Phase == "" {
			if err := d.send(model.Delta{Kind: model.MessageCategory, MessageID: item.ID, Category: cat}); err != nil {
				return err
			}
		}
		for i, p := range item.Content {
			if err := d.setPart(s, i, p, true); err != nil {
				return err
			}
		}
		if err := d.flush(s); err != nil {
			return err
		}
		full, err := itemText(item)
		if err != nil {
			return err
		}
		if full != s.text.String() {
			return wireError("final message lost content")
		}
	case "function_call", "tool_search_call":
		if item.CallID != s.item.CallID || item.Name != s.item.Name || item.Namespace != s.item.Namespace || item.Execution != s.item.Execution {
			return wireError("function call identity changed")
		}
		_, arguments, err := item.call(d.clientSearch)
		if err != nil {
			return err
		}
		var args map[string]any
		if decodeObject([]byte(arguments), &args) != nil {
			return wireError("function arguments must be a complete JSON object")
		}
		if item.Type == "tool_search_call" {
			if item.Status != "completed" {
				return wireError("native search call is not completed")
			}
			initial := s.item.Arguments
			if len(initial) != 0 && !bytes.Equal(initial, []byte("null")) && !sameJSON(initial, []byte("{}")) && !sameJSON(initial, item.Arguments) {
				return wireError("native search arguments changed")
			}
		}
		if err := d.arguments(s, arguments, true); err != nil {
			return err
		}
	}
	if d.responseID == "" {
		return wireError("output arrived without response identity")
	}
	version := 1
	if item.Type == "tool_search_call" || item.Type == "function_call" && item.Namespace != "" {
		version = 2
	}
	raw, err := json.Marshal(savedItem{Version: version, Endpoint: d.endpoint, Model: d.model, ResponseID: d.responseID, Item: e.Item})
	if err != nil {
		return err
	}
	if len(raw) > model.MaxBlockBytes {
		return protocol.E("output_limit", "Responses private item exceeds budget")
	}
	if err := d.budget(len(raw)); err != nil {
		return err
	}
	private := model.Delta{Kind: model.ProtocolData, Protocol: Protocol, Raw: raw}
	end := model.Delta{Kind: model.MessageEnd, MessageID: item.ID}
	if item.Type == "function_call" || item.Type == "tool_search_call" {
		private.CallID = item.CallID
		end = model.Delta{Kind: model.CallEnd, CallID: item.CallID}
	} else {
		private.MessageID = item.ID
	}
	if err := d.send(private); err != nil {
		return err
	}
	if err := d.send(end); err != nil {
		return err
	}
	s.done = true
	s.raw = append(json.RawMessage(nil), e.Item...)
	return nil
}

func (d *decoder) complete(raw []byte) (bool, error) {
	var response struct {
		ID     string            `json:"id"`
		Status string            `json:"status"`
		Error  json.RawMessage   `json:"error"`
		Output []json.RawMessage `json:"output"`
		Usage  *model.Usage      `json:"usage"`
	}
	if err := decodeObject(raw, &response); err != nil {
		return false, err
	}
	if err := d.identify(response.ID); err != nil {
		return false, err
	}
	if response.Status != "completed" || len(response.Error) != 0 && !bytes.Equal(response.Error, []byte("null")) {
		return false, wireError("completed event contains a failed response")
	}
	if len(response.Output) != len(d.items) {
		return false, wireError("completed response lost output items")
	}
	for i, s := range d.items {
		if !s.done || !sameJSON(s.raw, response.Output[i]) {
			return false, wireError("completed output differs from item stream")
		}
	}
	if response.Usage != nil {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens < 0 || response.Usage.OutputTokens != nil && *response.Usage.OutputTokens < 0 {
			return false, wireError("negative token usage")
		}
		if err := d.send(model.Delta{Kind: model.UsageUpdate, Usage: response.Usage}); err != nil {
			return false, err
		}
	}
	if err := d.send(model.Delta{Kind: model.ResponseEnd}); err != nil {
		return false, err
	}
	return true, nil
}
