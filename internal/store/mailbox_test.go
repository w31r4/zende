package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func mailAgent(t *testing.T, s *Store) protocol.Accepted {
	t.Helper()
	p := protocol.CreateAgent{Cwd: t.TempDir()}
	r, _ := protocol.NewRequest("agent.create", protocol.NewID("create"), p)
	a, err := s.Create(context.Background(), r, p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func sendMail(t *testing.T, s *Store, id, mode, text string) protocol.Accepted {
	t.Helper()
	p := protocol.SendMessage{AgentID: id, Delivery: mode, Input: text}
	r, _ := protocol.NewRequest("agent.send", protocol.NewID("send"), p)
	a, err := s.Send(context.Background(), r, p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func mailControl(t *testing.T, s *Store, method, id string) protocol.Accepted {
	t.Helper()
	p := protocol.Target{AgentID: id}
	r, _ := protocol.NewRequest("agent."+method, protocol.NewID(method), p)
	var a protocol.Accepted
	var err error
	if method == "stop" {
		a, err = s.Stop(context.Background(), r, p)
	} else {
		a, err = s.Resume(context.Background(), r, p, 1000)
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMailboxAcceptanceAttributionRetryAndReadOnlyQueries(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, sender := create(t, s)
	recipient := mailAgent(t, s)
	if _, err := s.ClaimNext(ctx, sender.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := protocol.Caller{AgentID: sender.AgentID, TaskID: sender.TaskID, OperationID: "command"}
	if err := s.AppendTaskEvent(ctx, sender.AgentID, sender.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "request", "call": model.ToolCall{ID: "call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	p := protocol.SendMessage{AgentID: recipient.AgentID, Input: "消息\nsecond line", Delivery: "queue", Caller: &caller}
	req, _ := protocol.NewRequest("agent.send", "send-once", p)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Send(ctx, req, p, 8); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	a, err := s.Send(ctx, req, p, 8)
	if err != nil || a.MessageID == "" || a.TaskID != "" {
		t.Fatal(a, err)
	}
	m, err := s.Mail(ctx, a.MessageID)
	if err != nil || m.Source == nil || m.Source.Caller != caller || m.State != "accepted" || m.Input != p.Input {
		t.Fatal(m, err)
	}
	before, _ := s.Snapshot(ctx, recipient.AgentID)
	for _, direction := range []string{"inbox", "outbox"} {
		id := recipient.AgentID
		if direction == "outbox" {
			id = sender.AgentID
		}
		page, err := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: id, Direction: direction})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != m.ID {
			t.Fatal(page, err)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), `"input":`) {
			t.Fatal("list expanded message body")
		}
	}
	after, _ := s.Snapshot(ctx, recipient.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("query changed state")
	}
	p.Caller = &protocol.Caller{AgentID: "later", TaskID: "later", OperationID: "later"}
	retry, _ := protocol.NewRequest("agent.send", req.OperationID, p)
	again, err := s.Send(ctx, retry, p, 0)
	if err != nil || !reflect.DeepEqual(again, a) {
		t.Fatal(again, err)
	}
	p.Input = "changed"
	bad, _ := protocol.NewRequest("agent.send", req.OperationID, p)
	if _, err := s.Send(ctx, bad, p, 8); err == nil {
		t.Fatal("changed message accepted")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.ClaimNext(ctx, recipient.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	m, err = s.Mail(ctx, a.MessageID)
	if err != nil || m.State != "delivered" || m.DeliveredSeq == 0 || m.TaskID != w.Task.ID {
		t.Fatal(m, err)
	}
	messages, _, err := s.ModelContext(ctx, recipient.AgentID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content[0].Text, `"sender_kind":"agent"`) || !strings.Contains(messages[0].Content[0].Text, sender.AgentID) {
		t.Fatal(messages, err)
	}
	if err := s.FinishTask(ctx, recipient.AgentID, w.Task.ID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mail(ctx, a.MessageID)
	if m.State != "processed" || m.TaskState != "completed" {
		t.Fatal(m)
	}
	if w, err := s.ClaimNext(ctx, recipient.AgentID); err != nil || w != nil {
		t.Fatal("redelivered", w, err)
	}
	for _, sql := range []string{"UPDATE mailbox SET input='changed'", "UPDATE mailbox SET task_id='changed'", "UPDATE mailbox SET delivered_seq=1", "DELETE FROM mailbox"} {
		if _, err := s.db.Exec(sql); err == nil {
			t.Fatal(sql)
		}
	}
}

func TestMailboxAtomicAcceptanceDeliveryAndSchemaNineMigration(t *testing.T) {
	for _, stage := range []string{"accept", "context", "cursor", "event"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newStore(t)
			a := mailAgent(t, s)
			p := protocol.SendMessage{AgentID: a.AgentID, Delivery: "queue", Input: "original"}
			req, _ := protocol.NewRequest("agent.send", "durable", p)
			if stage == "accept" {
				if _, err := s.db.Exec("CREATE TRIGGER injected BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Send(ctx, req, p, 8); err == nil {
					t.Fatal("accept succeeded")
				}
				page, _ := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: a.AgentID})
				if len(page.Items) != 0 {
					t.Fatal("partial mail", page)
				}
				if _, err := s.db.Exec("DROP TRIGGER injected"); err != nil {
					t.Fatal(err)
				}
			}
			accepted, err := s.Send(ctx, req, p, 8)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.Snapshot(ctx, a.AgentID)
			trigger := ""
			switch stage {
			case "context":
				trigger = "CREATE TRIGGER injected BEFORE INSERT ON events WHEN NEW.kind='context.message' BEGIN SELECT RAISE(ABORT,'fixture'); END"
			case "cursor":
				trigger = "CREATE TRIGGER injected BEFORE UPDATE OF delivered_seq ON mailbox BEGIN SELECT RAISE(ABORT,'fixture'); END"
			case "event":
				trigger = "CREATE TRIGGER injected BEFORE INSERT ON events WHEN NEW.kind='mailbox.delivered' BEGIN SELECT RAISE(ABORT,'fixture'); END"
			}
			if trigger != "" {
				if _, err := s.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ClaimNext(ctx, a.AgentID); err == nil {
					t.Fatal("delivery succeeded")
				}
				after, _ := s.Snapshot(ctx, a.AgentID)
				m, _ := s.Mail(ctx, accepted.MessageID)
				if !reflect.DeepEqual(before, after) || m.DeliveredSeq != 0 || m.TaskID != "" {
					t.Fatal("partial delivery", m)
				}
				if _, err := s.db.Exec("DROP TRIGGER injected"); err != nil {
					t.Fatal(err)
				}
			}
			if w, err := s.ClaimNext(ctx, a.AgentID); err != nil || w == nil {
				t.Fatal(w, err)
			}
		})
	}
	ctx := context.Background()
	s, path := newStore(t)
	_, a := create(t, s)
	before, _ := s.Snapshot(ctx, a.AgentID)
	if _, err := s.db.Exec("DROP TABLE plugin_progress; DROP TABLE plugin_calls; DROP TABLE mcp_calls; DROP TABLE resource_updates; DROP TABLE wait_receipts; DROP TABLE mailbox; PRAGMA user_version=9"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	after, _ := next.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed history")
	}
	sendMail(t, next, a.AgentID, "queue", "after upgrade")
}

func TestMailboxPauseResumeOrderPriorityAndRequestFence(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	q := sendMail(t, s, a.AgentID, "queue", "queued")
	st := sendMail(t, s, a.AgentID, "steer", "steer")
	view, _ := s.ContextSnapshot(ctx, a.AgentID)
	if err := s.BeginModelRequest(ctx, a.AgentID, a.TaskID, 0, view.Cursor, nil, func(int64) any { return map[string]string{"request_id": "blocked"} }); err == nil {
		t.Fatal("request skipped pending steer")
	}
	mailControl(t, s, "stop", a.AgentID)
	if err := s.DeliverSteer(ctx, a.AgentID, a.TaskID); err == nil {
		t.Fatal("pause bypassed")
	}
	if err := s.FinishTask(ctx, a.AgentID, a.TaskID, "completed", "late", nil); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ReadyAgents(ctx); err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	resumed := mailControl(t, s, "resume", a.AgentID)
	// Pausing again before ClaimNext must retain original acceptance order.
	mailControl(t, s, "stop", a.AgentID)
	again := mailControl(t, s, "resume", a.AgentID)
	if resumed.TaskID != again.TaskID {
		t.Fatal("resume reordered reserved mail", resumed, again)
	}
	w, err := s.ClaimNext(ctx, a.AgentID)
	if err != nil || w.Task.ID != resumed.TaskID {
		t.Fatal(w, err)
	}
	m, _ := s.Mail(ctx, q.MessageID)
	if m.TaskID != w.Task.ID || m.State != "delivered" {
		t.Fatal(m)
	}
	newSteer := sendMail(t, s, a.AgentID, "steer", "new after resume")
	if err := s.DeliverSteer(ctx, a.AgentID, w.Task.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mail(ctx, st.MessageID)
	if m.TaskID != "" || m.State != "accepted" {
		t.Fatal(m)
	}
	m, _ = s.Mail(ctx, newSteer.MessageID)
	if m.TaskID != w.Task.ID || m.State != "delivered" {
		t.Fatal(m)
	}
	if err := s.DeliverSteer(ctx, a.AgentID, w.Task.ID); err != nil {
		t.Fatal(err)
	}
	page, _ := s.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	count := 0
	for _, e := range page.Events {
		if e.Kind == "mailbox.delivered" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("duplicate delivery", count)
	}
	if err := s.FinishTask(ctx, a.AgentID, w.Task.ID, "completed", "queue done", nil); err != nil {
		t.Fatal(err)
	}
	w, err = s.ClaimNext(ctx, a.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	m, _ = s.Mail(ctx, st.MessageID)
	if m.TaskID != w.Task.ID || m.State != "delivered" {
		t.Fatal(m)
	}
	mailControl(t, s, "stop", a.AgentID)
	if err := s.FinishTask(ctx, a.AgentID, w.Task.ID, "completed", "", nil); err != nil {
		t.Fatal(err)
	}
	later := sendMail(t, s, a.AgentID, "queue", "later")
	p := protocol.Submit{AgentID: a.AgentID, Input: "explicit new input"}
	r, _ := protocol.NewRequest("agent.submit", "priority", p)
	input, err := s.Submit(ctx, r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.ClaimNext(ctx, a.AgentID)
	if err != nil || w.Task.ID != input.TaskID {
		t.Fatal(w, err)
	}
	if m, _ := s.Mail(ctx, later.MessageID); m.State != "accepted" {
		t.Fatal("queue bypassed explicit input")
	}
	if err := s.FinishTask(ctx, a.AgentID, w.Task.ID, "unknown", "", protocol.E("operation_unknown", "fixture")); err != nil {
		t.Fatal(err)
	}
	sendMail(t, s, a.AgentID, "steer", "held")
	if ids, _ := s.ReadyAgents(ctx); len(ids) != 0 {
		t.Fatal("unknown bypassed")
	}
}

func TestMailboxValidationPaginationAndCapacity(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	a := mailAgent(t, s)
	for _, p := range []protocol.SendMessage{
		{AgentID: a.AgentID, Input: ""}, {AgentID: a.AgentID, Input: "x", Delivery: "bad"},
		{AgentID: a.AgentID, Input: strings.Repeat("x", protocol.MaxMailboxBytes+1)},
		{AgentID: a.AgentID, Input: string([]byte{255})}, {AgentID: "absent", Input: "x"},
		{AgentID: a.AgentID, Input: "x", Caller: &protocol.Caller{AgentID: a.AgentID, TaskID: "bad", OperationID: "bad"}},
	} {
		r, _ := protocol.NewRequest("agent.send", protocol.NewID("invalid"), p)
		if _, err := s.Send(ctx, r, p, 8); err == nil {
			t.Fatal(p)
		}
	}
	var ids []string
	for range 5 {
		ids = append(ids, sendMail(t, s, a.AgentID, "queue", "中文").MessageID)
	}
	var got []string
	var cursor int64
	for {
		page, err := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: a.AgentID, After: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Items {
			got = append(got, m.ID)
			if m.Bytes != len("中文") {
				t.Fatal(m)
			}
		}
		cursor = page.Next
		if !page.More {
			break
		}
	}
	if !reflect.DeepEqual(ids, got) {
		t.Fatal(got)
	}
	b := mailAgent(t, s)
	p := protocol.SendMessage{AgentID: b.AgentID, Input: "capacity"}
	r, _ := protocol.NewRequest("agent.send", "capacity", p)
	if _, err := s.Send(ctx, r, p, 1); err == nil {
		t.Fatal("pending mailbox not counted")
	}
	for i := 5; i < protocol.MaxPendingMail; i++ {
		sendMail(t, s, a.AgentID, "queue", "x")
	}
	p.AgentID = a.AgentID
	r, _ = protocol.NewRequest("agent.send", "overflow", p)
	if _, err := s.Send(ctx, r, p, 8); err == nil {
		t.Fatal("unbounded mailbox")
	}
	for _, q := range []protocol.MailboxQuery{{AgentID: a.AgentID, Limit: 101}, {AgentID: a.AgentID, After: 999999}, {AgentID: a.AgentID, State: "bad"}, {AgentID: a.AgentID, Direction: "bad"}, {AgentID: "missing"}} {
		if _, err := s.Mailbox(ctx, q); err == nil {
			t.Fatal(q)
		}
	}
}

func TestMailboxSteerBatchRollbackAndSelfSource(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, a := create(t, s)
	if _, err := s.ClaimNext(ctx, a.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := protocol.Caller{AgentID: a.AgentID, TaskID: a.TaskID, OperationID: "self-command"}
	if err := s.AppendTaskEvent(ctx, a.AgentID, a.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "request", "call": model.ToolCall{ID: "call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	p := protocol.SendMessage{AgentID: a.AgentID, Input: "self", Delivery: "steer", Caller: &caller}
	req, _ := protocol.NewRequest("agent.send", "self", p)
	self, err := s.Send(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Mail(ctx, self.MessageID)
	if err != nil || m.SentSeq != m.Seq+1 {
		t.Fatal(m, err)
	}
	in, _ := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: a.AgentID})
	out, _ := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: a.AgentID, Direction: "outbox"})
	if len(in.Items) != 1 || len(out.Items) != 1 || in.Items[0].ID != out.Items[0].ID {
		t.Fatal(in, out)
	}
	second := sendMail(t, s, a.AgentID, "steer", "second")
	before, _ := s.ContextSnapshot(ctx, a.AgentID)
	trigger := "CREATE TRIGGER injected BEFORE UPDATE OF delivered_seq ON mailbox WHEN OLD.id='" + second.MessageID + "' BEGIN SELECT RAISE(ABORT,'fixture'); END"
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverSteer(ctx, a.AgentID, a.TaskID); err == nil {
		t.Fatal("partial batch committed")
	}
	after, _ := s.ContextSnapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("context committed without all cursors")
	}
	for _, id := range []string{self.MessageID, second.MessageID} {
		m, _ := s.Mail(ctx, id)
		if m.State != "accepted" || m.TaskID != "" {
			t.Fatal(m)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER injected"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverSteer(ctx, a.AgentID, a.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverSteer(ctx, a.AgentID, a.TaskID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Mail(ctx, self.MessageID)
	last, _ := s.Mail(ctx, second.MessageID)
	if first.DeliveredSeq == 0 || last.DeliveredSeq <= first.DeliveredSeq {
		t.Fatal(first, last)
	}
}
