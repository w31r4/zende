package render

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const (
	transcriptEntries = 512
	transcriptBytes   = 4 << 20
	entryBytes        = 64 << 10
)

// Entry is an observed fact, not a task scheduler or a second model protocol.
// Sequence and identities refer to the original append-only core history.
type Entry struct {
	Key, AgentID, TaskID, RequestID, MessageID string
	OperationID, CallID, ProcessID, Name       string
	Kind, Category, State, Text                string
	Attachments                                []string
	Skills                                     []protocol.ResourceRef
	Mail                                       *protocol.MailInfo
	Index                                      int
	Seq, LastSeq                               int64
	Truncated, Hidden                          bool
}

// Transcript retains a bounded display window. Eviction never writes to core.
// DroppedThrough explicitly distinguishes a window from complete history.
type Transcript struct {
	Agent          protocol.Agent
	ResolvedModel  string
	Cursor         int64
	Entries        []Entry
	DroppedThrough int64
	bytes          int
}

func (p *Transcript) entry(e protocol.Event, key, kind string) *Entry {
	for i := range p.Entries {
		if p.Entries[i].Key == key {
			p.Entries[i].LastSeq = e.Seq
			return &p.Entries[i]
		}
	}
	p.Entries = append(p.Entries, Entry{Key: key, Kind: kind, AgentID: e.AgentID, TaskID: e.TaskID, Seq: e.Seq, LastSeq: e.Seq})
	return &p.Entries[len(p.Entries)-1]
}

func (p *Transcript) text(entry *Entry, text string, appendText bool) {
	p.bytes -= len(entry.Text)
	if appendText {
		text = entry.Text + text
	}
	if len(text) > entryBytes {
		start := len(text) - entryBytes
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		text = text[start:]
		entry.Truncated = true
	}
	// Clone releases the original, possibly multi-megabyte event allocation.
	entry.Text = strings.Clone(text)
	p.bytes += len(entry.Text)
}

func (p *Transcript) trim() {
	for len(p.Entries) > transcriptEntries || p.bytes > transcriptBytes {
		p.bytes -= len(p.Entries[0].Text)
		p.bytes -= mailBytes(p.Entries[0].Mail)
		for _, id := range p.Entries[0].Attachments {
			p.bytes -= len(id)
		}
		for _, ref := range p.Entries[0].Skills {
			p.bytes -= len(ref.ID) + len(ref.Version)
		}
		p.DroppedThrough = max(p.DroppedThrough, p.Entries[0].LastSeq)
		p.Entries[0] = Entry{}
		p.Entries = p.Entries[1:]
	}
}

func validID(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

func validSkillRefs(refs []protocol.ResourceRef) bool {
	if len(refs) > protocol.MaxInputSkills {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		hash, err := hex.DecodeString(ref.Version)
		if err != nil || len(hash) != 32 || ref.ID == "" || len(ref.ID) > 128 || seen[ref.ID] {
			return false
		}
		seen[ref.ID] = true
	}
	return true
}
func (p *Transcript) skills(entry *Entry, refs []protocol.ResourceRef) {
	for _, ref := range entry.Skills {
		p.bytes -= len(ref.ID) + len(ref.Version)
	}
	entry.Skills = append([]protocol.ResourceRef(nil), refs...)
	for _, ref := range entry.Skills {
		p.bytes += len(ref.ID) + len(ref.Version)
	}
}

func decodePayload(e protocol.Event, target any) error {
	if err := json.Unmarshal(e.Payload, target); err != nil {
		return protocol.E("invalid_state", "invalid payload for "+e.Kind)
	}
	return nil
}

// Apply advances only a contiguous, valid stream. Unknown event kinds advance
// the cursor without exposing raw/private fields as ordinary model text.
func (p *Transcript) Apply(e protocol.Event) error {
	if e.Version != protocol.Version || e.AgentID != p.Agent.ID {
		return protocol.E("invalid_state", "unexpected event identity")
	}
	if e.Seq <= p.Cursor {
		return nil
	}
	if e.Seq != p.Cursor+1 {
		return protocol.E("cursor_gap", "event history has a gap")
	}
	if e.CopiedFrom != nil {
		if !validID(e.CopiedFrom.AgentID) || e.CopiedFrom.Seq <= 0 {
			return protocol.E("invalid_state", "invalid copied history source")
		}
		// Payloads keep their original identities. They may update historical
		// entries, but never the target's current model or execution state.
		agent, resolved := p.Agent, p.ResolvedModel
		original := e
		original.AgentID = e.CopiedFrom.AgentID
		err := p.apply(original)
		p.Agent, p.ResolvedModel = agent, resolved
		if err != nil {
			return err
		}
	} else {
		if err := p.apply(e); err != nil {
			return err
		}
	}
	p.Cursor = e.Seq
	p.Agent.Seq = e.Seq
	p.trim()
	return nil
}

func (p *Transcript) apply(e protocol.Event) error {
	switch e.Kind {
	case "context.compaction.state":
		var info protocol.CompactionInfo
		if err := decodePayload(e, &info); err != nil {
			return err
		}
		if !validID(info.OperationID) || info.AgentID != e.AgentID || info.Reason != "manual" && info.Reason != "automatic" || info.MaxAttempts < 1 || info.MaxAttempts > 8 || info.Attempts < 0 || info.Attempts > info.MaxAttempts {
			return protocol.E("invalid_state", "invalid compaction state")
		}
		switch info.State {
		case "pending", "running":
		case "completed":
			if !validID(info.ViewID) {
				return protocol.E("invalid_state", "completed compaction has no view")
			}
		case "failed", "cancelled":
			if info.Failure == nil {
				return protocol.E("invalid_state", "compaction failure has no reason")
			}
		default:
			return protocol.E("invalid_state", "unknown compaction state")
		}
		entry := p.entry(e, "compaction:"+info.OperationID, "compaction")
		entry.OperationID, entry.TaskID, entry.RequestID = info.OperationID, info.TaskID, info.RequestID
		entry.State = info.State
		p.text(entry, "context "+info.State, false)
	case "mailbox.accepted", "mailbox.delivered":
		return p.applyMail(e)
	case "question.asked", "question.answered", "question.cancelled":
		var q protocol.Question
		if err := decodePayload(e, &q); err != nil {
			return err
		}
		state := map[string]string{"question.asked": "pending", "question.answered": "answered", "question.cancelled": "cancelled"}[e.Kind]
		if !validQuestion(q.QuestionInfo, e.AgentID) || q.TaskID != e.TaskID || q.State != state ||
			len(q.Answer) > protocol.MaxAnswerBytes || !utf8.ValidString(q.Answer) || len(q.Attachments) > protocol.MaxInputImages || !validSkillRefs(q.Skills) {
			return protocol.E("invalid_state", "invalid question event")
		}
		if q.State != "answered" && (q.Answer != "" || len(q.Attachments) > 0 || len(q.Skills) > 0) ||
			q.State == "answered" && strings.TrimSpace(q.Answer) == "" && len(q.Attachments) == 0 && len(q.Skills) == 0 {
			return protocol.E("invalid_state", "invalid question answer")
		}
		seen := make(map[string]bool)
		for _, id := range q.Attachments {
			if id == "" || len(id) > 128 || seen[id] {
				return protocol.E("invalid_state", "invalid answer image reference")
			}
			seen[id] = true
		}
		entry := p.entry(e, "question:"+q.ID, "question")
		entry.State = q.State
		p.text(entry, q.Prompt, false)
		if q.State == "answered" {
			entry = p.entry(e, "answer:"+q.ID, "user")
			entry.State = q.State
			for _, id := range entry.Attachments {
				p.bytes -= len(id)
			}
			entry.Attachments = append([]string(nil), q.Attachments...)
			for _, id := range entry.Attachments {
				p.bytes += len(id)
			}
			p.text(entry, q.Answer, false)
			p.skills(entry, q.Skills)
		}
	case "task.config", "model.started":
		// Only the public model ID is used; endpoint snapshots and credentials
		// remain outside the display projection.
		var data struct {
			Model string `json:"model"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		if data.Model != "" {
			p.ResolvedModel = strings.Clone(data.Model)
		}
	case "agent.created":
		var agent protocol.Agent
		if err := decodePayload(e, &agent); err != nil {
			return err
		}
		if agent.ID != e.AgentID {
			return protocol.E("invalid_state", "Agent payload identity mismatch")
		}
		p.Agent = agent
	case "agent.paused", "agent.resumed":
		var data struct {
			Paused bool   `json:"paused"`
			State  string `json:"state"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		p.Agent.Paused, p.Agent.State = data.Paused, data.State
	case "task.accepted", "task.started":
		var task protocol.Task
		if err := decodePayload(e, &task); err != nil {
			return err
		}
		if task.ID != e.TaskID || task.AgentID != e.AgentID || task.ID == "" {
			return protocol.E("invalid_state", "task payload identity mismatch")
		}
		if len(task.Attachments) > protocol.MaxInputImages {
			return protocol.E("invalid_state", "too many image references")
		}
		if !validSkillRefs(task.Skills) {
			return protocol.E("invalid_state", "invalid skill references")
		}
		seen := make(map[string]bool)
		for _, id := range task.Attachments {
			if id == "" || len(id) > 128 || seen[id] {
				return protocol.E("invalid_state", "invalid image reference")
			}
			seen[id] = true
		}
		entry := p.entry(e, "task:"+e.TaskID, "user")
		entry.State = task.State
		for _, id := range entry.Attachments {
			p.bytes -= len(id)
		}
		entry.Attachments = append([]string(nil), task.Attachments...)
		for _, id := range entry.Attachments {
			p.bytes += len(id)
		}
		p.text(entry, task.Input, false)
		p.skills(entry, task.Skills)
		if e.Kind == "task.started" {
			p.Agent.State = "running"
		}
	case "task.completed", "task.failed", "task.cancelled", "task.unknown", "task.cancelling", "task.cancellation_pending", "task.recovered":
		var data struct {
			State  string          `json:"state"`
			Reason string          `json:"reason"`
			Error  *protocol.Error `json:"error"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		state := strings.TrimPrefix(e.Kind, "task.")
		if state == "recovered" {
			state = data.State
		}
		p.entry(e, "task:"+e.TaskID, "user").State = state
		for i := range p.Entries {
			entry := &p.Entries[i]
			if entry.Mail != nil && entry.TaskID == e.TaskID && entry.Mail.DeliveredSeq > 0 {
				entry.Mail.TaskState, entry.LastSeq = state, e.Seq
				entry.State = "delivered"
				if state == "completed" {
					entry.State = "processed"
				}
				entry.Mail.State = entry.State
			}
		}
		switch state {
		case "completed", "failed", "cancelled":
			p.Agent.State = "idle"
			if p.Agent.Paused {
				p.Agent.State = "paused"
			}
		case "unknown", "cancellation_pending":
			p.Agent.State = "unknown"
		case "running", "cancelling":
			p.Agent.State = state
		}
		if state != "running" && state != "completed" {
			text := state
			if data.Error != nil {
				text += ": " + data.Error.Error()
			} else if data.Reason != "" {
				text += ": " + data.Reason
			}
			p.notice(e, text)
		}
		if state == "unknown" || state == "failed" || state == "cancelled" || state == "cancellation_pending" {
			for i := range p.Entries {
				entry := &p.Entries[i]
				if entry.TaskID == e.TaskID && entry.Kind == "assistant" && entry.State == "streaming" {
					entry.State = "interrupted"
				}
			}
		}
	case "model.delta":
		var data struct {
			RequestID string      `json:"request_id"`
			Delta     model.Delta `json:"delta"`
			TextBytes []byte      `json:"text_bytes"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		if data.TextBytes != nil {
			data.Delta.Text = string(data.TextBytes)
		}
		// Never display provider continuation, reasoning, or tool-argument data.
		if data.Delta.Kind != model.TextDelta || data.Delta.Text == "" {
			return nil
		}
		entry := p.entry(e, "raw:"+data.RequestID+":"+data.Delta.MessageID, "assistant")
		entry.RequestID, entry.MessageID = data.RequestID, data.Delta.MessageID
		entry.Category, entry.State = "unclassified", "streaming"
		p.text(entry, data.Delta.Text, true)
		for _, other := range p.Entries {
			if other.RequestID == entry.RequestID && other.MessageID == entry.MessageID && other.Category != "unclassified" && other.Kind == "assistant" {
				entry.Hidden = true
				break
			}
		}
	case "model.output.delta":
		var data struct {
			RequestID string            `json:"request_id"`
			Delta     model.PublicDelta `json:"delta"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		d := data.Delta
		if data.RequestID == "" || d.MessageID == "" || d.Index < 0 || (d.Category != "commentary" && d.Category != "final") || (d.Kind != "start" && d.Kind != "text" && d.Kind != "end") {
			return protocol.E("invalid_state", "invalid public output delta")
		}
		entry := p.output(e, data.RequestID, d.MessageID, d.Index)
		entry.Category, entry.State = d.Category, "streaming"
		if d.Kind == "text" {
			p.text(entry, d.Text, true)
		}
		p.hideRaw(data.RequestID, d.MessageID)
	case "model.completed":
		var data struct {
			RequestID string                `json:"request_id"`
			Output    []model.PublicMessage `json:"output"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		for _, output := range data.Output {
			if data.RequestID == "" || output.MessageID == "" || output.Index < 0 || (output.Category != "commentary" && output.Category != "final") {
				return protocol.E("invalid_state", "invalid confirmed output category")
			}
		}
		for _, output := range data.Output {
			entry := p.output(e, data.RequestID, output.MessageID, output.Index)
			entry.Category, entry.State, entry.Truncated = output.Category, "confirmed", false
			p.text(entry, output.Text, false)
			p.hideRaw(data.RequestID, output.MessageID)
		}
	case "model.output.invalid", "model.interrupted":
		var data struct {
			RequestID string          `json:"request_id"`
			Reason    string          `json:"reason"`
			Error     *protocol.Error `json:"error"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		state := "invalid"
		if e.Kind == "model.interrupted" {
			state = "interrupted"
		}
		for i := range p.Entries {
			if p.Entries[i].RequestID == data.RequestID && p.Entries[i].Kind == "assistant" {
				p.Entries[i].State = state
			}
		}
		text := data.Reason
		if data.Error != nil {
			text = data.Error.Error()
		}
		p.notice(e, state+": "+text)
	case "tool.started", "tool.dispatching", "tool.rejected", "tool.skipped", "tool.completed", "tool.unknown":
		return p.applyTool(e)
	case "recovery.held":
		var data struct {
			Error *protocol.Error `json:"error"`
		}
		if err := decodePayload(e, &data); err != nil {
			return err
		}
		if data.Error != nil {
			p.notice(e, data.Error.Error())
		}
	}
	return nil
}

func (p *Transcript) output(e protocol.Event, request, message string, index int) *Entry {
	entry := p.entry(e, fmt.Sprintf("output:%s:%s:%d", request, message, index), "assistant")
	entry.RequestID, entry.MessageID, entry.Index = request, message, index
	return entry
}

func (p *Transcript) hideRaw(request, message string) {
	for i := range p.Entries {
		entry := &p.Entries[i]
		if entry.RequestID == request && entry.MessageID == message && entry.Category == "unclassified" {
			entry.Hidden = true
		}
	}
}

func (p *Transcript) notice(e protocol.Event, text string) {
	entry := p.entry(e, fmt.Sprintf("event:%d", e.Seq), "notice")
	entry.State = e.Kind
	p.text(entry, text, false)
}

func (p *Transcript) applyTool(e protocol.Event) error {
	type invocation struct {
		ID        string         `json:"operation_id"`
		RequestID string         `json:"request_id"`
		Call      model.ToolCall `json:"call"`
	}
	var data struct {
		invocation
		Invocation invocation `json:"invocation"`
		Result     struct {
			Content   []model.Block `json:"content"`
			ProcessID string        `json:"process_id"`
		} `json:"result"`
		Error          *protocol.Error `json:"error"`
		Reason         string          `json:"reason"`
		ResultMessages []string        `json:"result_messages"`
	}
	if err := decodePayload(e, &data); err != nil {
		return err
	}
	op := data.invocation
	if data.Invocation.ID != "" {
		op = data.Invocation
	}
	if op.ID == "" {
		return protocol.E("invalid_state", "tool event has no operation identity")
	}
	if len(data.ResultMessages) > len(data.Result.Content) {
		return protocol.E("invalid_state", "tool result message references exceed content")
	}
	seen := map[string]bool{}
	for _, id := range data.ResultMessages {
		if id == "" || seen[id] {
			return protocol.E("invalid_state", "invalid tool result message reference")
		}
		seen[id] = true
	}
	entry := p.entry(e, "tool:"+op.ID, "tool")
	entry.OperationID = op.ID
	if op.RequestID != "" {
		entry.RequestID = op.RequestID
	}
	if op.Call.ID != "" {
		entry.CallID, entry.Name = op.Call.ID, op.Call.Name
	}
	entry.State = strings.TrimPrefix(e.Kind, "tool.")
	if data.Result.ProcessID != "" {
		entry.ProcessID = data.Result.ProcessID
	}
	var text strings.Builder
	// Host-added result blocks are already shown by mailbox.accepted. The
	// explicit suffix mapping avoids guessing from arbitrary shell output.
	for _, block := range data.Result.Content[:len(data.Result.Content)-len(data.ResultMessages)] {
		if block.Type == "text" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text)
		}
	}
	if data.Error != nil {
		if entry.State == "completed" {
			entry.State = "failed"
		}
		text.WriteString("\n" + data.Error.Error())
	} else if data.Reason != "" {
		text.WriteString(data.Reason)
	}
	if text.Len() > 0 || e.Kind == "tool.completed" {
		p.text(entry, text.String(), false)
	}
	return nil
}
