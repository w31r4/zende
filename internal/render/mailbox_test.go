package render

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func TestMailboxProjectionAttributionLifecycleAndWidth(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	mail := protocol.Mail{MailInfo: protocol.MailInfo{ID: "message", AgentID: "agent", OperationID: "op", SenderKind: "agent", Delivery: "steer", Seq: 1, State: "accepted",
		Source: &protocol.CallSource{Caller: protocol.Caller{AgentID: "peer", TaskID: "peer-task", OperationID: "exec"}, RequestID: "request", CallID: strings.Repeat("c", 256)}},
		Input: "检查中文 é 🙂 \x1b[2J"}
	mail.Bytes = len(mail.Input)
	appendProjection(t, &p, "mailbox.accepted", mail)
	if got := strings.Join(p.Lines(80, 24, 0, false), "\n"); !strings.Contains(got, "@ peer · steer · accepted") || strings.Contains(got, "> ") || strings.ContainsRune(got, '\x1b') {
		t.Fatal(got)
	}
	task := protocol.Task{ID: "task", AgentID: "agent", State: "accepted"}
	appendProjection(t, &p, "task.accepted", task)
	task.State = "running"
	appendProjection(t, &p, "task.started", task)
	appendProjection(t, &p, "context.message", map[string]string{"private": "not a second displayed input"})
	mail.TaskID, mail.DeliveredSeq, mail.State, mail.TaskState = "task", p.Cursor, "delivered", "running"
	appendProjection(t, &p, "mailbox.delivered", mail.MailInfo)
	if p.Entries[0].Mail.Source.CallID != mail.Source.CallID || p.Entries[0].TaskID != "task" || p.Entries[0].State != "delivered" {
		t.Fatal(p.Entries)
	}
	appendProjection(t, &p, "task.unknown", map[string]string{"state": "unknown"})
	if p.Entries[0].State != "delivered" || p.Entries[0].Mail.TaskState != "unknown" {
		t.Fatal("unknown work falsely processed", p.Entries[0])
	}
	appendProjection(t, &p, "task.recovered", map[string]string{"state": "running"})
	appendProjection(t, &p, "task.completed", map[string]string{"state": "completed"})
	if p.Entries[0].State != "processed" || p.Entries[0].Mail.TaskState != "completed" || p.Entries[0].LastSeq != p.Cursor {
		t.Fatal(p.Entries)
	}
	before, _ := json.Marshal(p)
	for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
		m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
		m.transcript = p
		m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		if strings.Count(view, "检查中文") != 1 || !strings.Contains(view, "@ peer") || !strings.Contains(view, "processed") {
			t.Fatal(size, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("mailbox overflow", size, line)
			}
		}
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("render changed facts")
	}
	// A later metadata event cannot invent a replacement for evicted body text.
	p.Entries, p.bytes = nil, 0
	appendProjection(t, &p, "mailbox.delivered", mail.MailInfo)
	if len(p.Entries) != 0 {
		t.Fatal("evicted body resurrected")
	}
}

func TestMailboxProjectionRejectsMalformedEventsAndBoundsRetention(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	valid := protocol.Mail{MailInfo: protocol.MailInfo{ID: "message", AgentID: "agent", OperationID: "op", SenderKind: "user", Delivery: "queue", Seq: 1, State: "accepted", Bytes: 2}, Input: "ok"}
	cases := []protocol.Mail{valid, valid, valid, valid}
	cases[0].Bytes = 3
	cases[1].SenderKind = "agent"
	cases[2].State = "processed"
	cases[3].Seq = 2
	before, _ := json.Marshal(p)
	for _, mail := range cases {
		raw, _ := json.Marshal(mail)
		if err := p.Apply(protocol.Event{Version: 1, AgentID: "agent", Seq: 1, Kind: "mailbox.accepted", Payload: raw}); err == nil {
			t.Fatal("invalid mail accepted", mail)
		}
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("invalid event changed projection")
	}
	for i := 0; i < 100; i++ {
		mail := valid
		mail.ID, mail.Seq = fmt.Sprint("message-", i), p.Cursor+1
		mail.Input = strings.Repeat("中文", 12000)
		mail.Bytes = len(mail.Input)
		appendProjection(t, &p, "mailbox.accepted", mail)
	}
	if p.bytes > transcriptBytes || p.DroppedThrough == 0 {
		t.Fatal("unbounded mailbox history", p.bytes)
	}
	accounted := 0
	for _, entry := range p.Entries {
		accounted += len(entry.Text) + mailBytes(entry.Mail)
		if !entry.Truncated || !utf8.ValidString(entry.Text) {
			t.Fatal("invalid display truncation")
		}
	}
	if p.bytes != accounted {
		t.Fatal(p.bytes, accounted)
	}
}
