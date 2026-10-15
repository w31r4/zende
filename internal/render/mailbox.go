package render

import (
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

func (p *Transcript) applyMail(e protocol.Event) error {
	var m protocol.Mail
	if err := decodePayload(e, &m); err != nil {
		return err
	}
	validID := func(s string) bool { return s != "" && utf8.ValidString(s) }
	limit, minimum := protocol.MaxMailboxBytes, 1
	if m.Result != nil {
		limit, minimum = protocol.MaxTaskResultBytes, 0
	}
	if !validID(m.ID) || !validID(m.OperationID) || m.AgentID != e.AgentID ||
		m.Bytes < minimum || m.Bytes > limit ||
		(m.Delivery != "queue" && m.Delivery != "steer") ||
		(m.SenderKind != "user" && m.SenderKind != "agent") ||
		(m.SenderKind == "user") != (m.Source == nil && m.Result == nil) {
		return protocol.E("invalid_state", "invalid mailbox event")
	}
	if r := m.Result; r != nil && (!validID(r.AgentID) || !validID(r.TaskID) || r.Seq < 1 || m.Source != nil || m.Delivery != "queue") {
		return protocol.E("invalid_state", "invalid task result source")
	}
	if s := m.Source; s != nil && (!validID(s.Caller.AgentID) || !validID(s.Caller.TaskID) ||
		!validID(s.Caller.OperationID) || !validID(s.RequestID) || !validID(s.CallID)) {
		return protocol.E("invalid_state", "invalid mailbox sender")
	}
	if e.Kind == "mailbox.accepted" {
		if m.State != "accepted" || m.Seq != e.Seq || m.DeliveredSeq != 0 || m.TaskID != "" ||
			len(m.Input) != m.Bytes || !utf8.ValidString(m.Input) {
			return protocol.E("invalid_state", "invalid mailbox acceptance")
		}
		entry := p.entry(e, "mailbox:"+m.ID, "mailbox")
		p.bytes += mailBytes(&m.MailInfo) - mailBytes(entry.Mail)
		entry.MessageID, entry.OperationID, entry.State = m.ID, m.OperationID, m.State
		// Acceptance has no receiving task, even for a self-send.
		entry.TaskID, entry.Mail = "", &m.MailInfo
		p.text(entry, m.Input, false)
		return nil
	}
	if m.State != "delivered" || m.Seq < 1 || m.DeliveredSeq <= m.Seq ||
		m.DeliveredSeq >= e.Seq || !validID(m.TaskID) || m.TaskID != e.TaskID || m.Input != "" {
		return protocol.E("invalid_state", "invalid mailbox delivery")
	}
	for i := range p.Entries {
		entry := &p.Entries[i]
		if entry.Key == "mailbox:"+m.ID {
			p.bytes += mailBytes(&m.MailInfo) - mailBytes(entry.Mail)
			entry.Mail, entry.TaskID, entry.State, entry.LastSeq = &m.MailInfo, m.TaskID, m.State, e.Seq
			break
		}
	}
	// An evicted message stays in history. A metadata-only delivery must not
	// resurrect it as an empty or invented input in the bounded display window.
	return nil
}

func mailBytes(m *protocol.MailInfo) int {
	if m == nil {
		return 0
	}
	n := len(m.ID) + len(m.AgentID) + len(m.OperationID) + len(m.TaskID)
	if m.Result != nil {
		n += len(m.Result.AgentID) + len(m.Result.TaskID)
	}
	if s := m.Source; s != nil {
		n += len(s.Caller.AgentID) + len(s.Caller.TaskID) + len(s.Caller.OperationID) + len(s.RequestID) + len(s.CallID)
	}
	return n
}
