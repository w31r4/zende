package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func resultChild(t *testing.T, s *Store, parent string) protocol.Accepted {
	t.Helper()
	p := protocol.SpawnAgent{ParentID: parent, Input: "independent work", Context: "none"}
	r, _ := protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), p)
	selection, err := s.Selection(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Spawn(context.Background(), r, p, selection.Choice.Cwd, selection.Choice.Revision, 1000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := s.ClaimNext(context.Background(), a.AgentID); err != nil || w == nil {
		t.Fatal(w, err)
	}
	return a
}

func resultCaller(t *testing.T, s *Store, a protocol.Accepted, id string) *protocol.Caller {
	t.Helper()
	c := &protocol.Caller{AgentID: a.AgentID, TaskID: a.TaskID, OperationID: id}
	if err := s.AppendTaskEvent(context.Background(), a.AgentID, a.TaskID, "tool.dispatching", map[string]any{"operation_id": id, "request_id": "request", "call": model.ToolCall{ID: id, Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	return c
}

func resultToolMessage(mail ...protocol.Mail) model.Message {
	m := model.Message{Role: "tool", Content: []model.Block{{Type: "text", Text: "process observation", CallID: "call"}}}
	for _, v := range mail {
		m.Content = append(m.Content, model.Block{Type: "text", Text: protocol.MailResultText(v), CallID: "call"})
	}
	return m
}

func TestTaskResultAtomicReturnWaitReceiptAndContextConsumption(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	_, parent := create(t, s)
	if _, err := s.ClaimNext(ctx, parent.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := resultCaller(t, s, parent, "process")
	child := resultChild(t, s, parent.AgentID)
	if _, err := s.db.Exec("CREATE TRIGGER injected BEFORE INSERT ON mailbox WHEN NEW.result_task IS NOT NULL BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "result 中文", nil); err == nil {
		t.Fatal("partial completion accepted")
	}
	snap, _ := s.Snapshot(ctx, child.AgentID)
	if snap.Tasks[0].State != "running" {
		t.Fatal(snap)
	}
	s.db.Exec("DROP TRIGGER injected")
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "result 中文", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "duplicate", nil); err == nil {
		t.Fatal("terminal changed")
	}
	beforeHuman, _ := s.Snapshot(ctx, parent.AgentID)
	if out, err := s.ObserveTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID}); err != nil || !out.Terminal {
		t.Fatal(out, err)
	}
	afterHuman, _ := s.Snapshot(ctx, parent.AgentID)
	var humanReceipts int
	if err := s.db.QueryRow("SELECT count(*) FROM wait_receipts").Scan(&humanReceipts); err != nil || humanReceipts != 0 || !reflect.DeepEqual(beforeHuman, afterHuman) {
		t.Fatal("human wait changed delivery", humanReceipts, err)
	}
	wait := protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller}
	out, err := s.ObserveTask(ctx, wait)
	if err != nil || !out.Terminal || out.State != "completed" || out.Result == nil || out.MessageID == "" || out.Bytes != len("result 中文") {
		t.Fatal(out, err)
	}
	for range 3 {
		again, err := s.ObserveTask(ctx, wait)
		if err != nil || !reflect.DeepEqual(again, out) {
			t.Fatal(again, err)
		}
	}
	var receipts int
	s.db.QueryRow("SELECT count(*) FROM wait_receipts").Scan(&receipts)
	if receipts != 1 {
		t.Fatal(receipts)
	}
	m, err := s.Mail(ctx, out.MessageID)
	if err != nil || m.State != "accepted" || m.TaskID != "" || m.Source != nil || m.Result.TaskID != child.TaskID || m.Result.AgentID != child.AgentID {
		t.Fatal(m, err)
	}
	// Returning a reference to the CLI does not count as model delivery.
	if err := s.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", map[string]string{"result": "only ref"}, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "call", Text: out.MessageID}}}, ToolMailDelivery{ProcessID: "process", Messages: []string{m.ID}}); err == nil {
		t.Fatal("reference consumed result")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.WaitedResults(ctx, parent.AgentID, "process")
	if err != nil || len(pending) != 1 || pending[0].ID != m.ID {
		t.Fatal(pending, err)
	}
	before, _ := s.Snapshot(ctx, parent.AgentID)
	if _, err := s.db.Exec("CREATE TRIGGER injected BEFORE UPDATE OF delivered_seq ON mailbox BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	message := resultToolMessage(m)
	delivery := ToolMailDelivery{ProcessID: "process", Messages: []string{m.ID}}
	if err := s.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", map[string]string{"result": "complete"}, message, delivery); err == nil {
		t.Fatal("partial context accepted")
	}
	after, _ := s.Snapshot(ctx, parent.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed consumption changed history")
	}
	s.db.Exec("DROP TRIGGER injected")
	if err := s.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", map[string]string{"result": "complete"}, message, delivery); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mail(ctx, m.ID)
	if m.State != "delivered" || m.TaskID != parent.TaskID || m.DeliveredSeq == 0 {
		t.Fatal(m)
	}
	deliveredSnapshot, _ := s.Snapshot(ctx, parent.AgentID)
	if err := s.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", nil, message, delivery); err == nil {
		t.Fatal("stale wait delivery duplicated context")
	}
	unchangedSnapshot, _ := s.Snapshot(ctx, parent.AgentID)
	if !reflect.DeepEqual(deliveredSnapshot, unchangedSnapshot) {
		t.Fatal("duplicate delivery changed history")
	}
	pending, err = s.WaitedResults(ctx, parent.AgentID, "process")
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if err := s.FinishTask(ctx, parent.AgentID, parent.TaskID, "completed", "parent done", nil); err != nil {
		t.Fatal(err)
	}
	if w, err := s.ClaimNext(ctx, parent.AgentID); err != nil || w != nil {
		t.Fatal("duplicate processing task", w, err)
	}
}

func TestTaskResultPauseQueueAndNoAutomaticAcknowledgement(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	root := mailAgent(t, s)
	middle := resultChild(t, s, root.AgentID)
	leaf := resultChild(t, s, middle.AgentID)
	if err := s.FinishTask(ctx, middle.AgentID, middle.TaskID, "completed", "middle work", nil); err != nil {
		t.Fatal(err)
	}
	mailControl(t, s, "stop", middle.AgentID)
	if err := s.FinishTask(ctx, leaf.AgentID, leaf.TaskID, "completed", "leaf result", nil); err != nil {
		t.Fatal(err)
	}
	page, _ := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: middle.AgentID})
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	if w, err := s.ClaimNext(ctx, middle.AgentID); err != nil || w != nil {
		t.Fatal("result woke pause", w, err)
	}
	mailControl(t, s, "resume", middle.AgentID)
	w, err := s.ClaimNext(ctx, middle.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	if err := s.FinishTask(ctx, middle.AgentID, w.Task.ID, "completed", "not an automatic reply", nil); err != nil {
		t.Fatal(err)
	}
	rootMail, _ := s.Mailbox(ctx, protocol.MailboxQuery{AgentID: root.AgentID})
	if len(rootMail.Items) != 1 || rootMail.Items[0].Result.TaskID != middle.TaskID {
		t.Fatal("notification generated acknowledgement", rootMail)
	}
	// A new business task, even one which waits for a child result, still returns
	// its own final. Suppression belongs to the notification-created task only.
	input := protocol.Submit{AgentID: middle.AgentID, Input: "new work"}
	req, _ := protocol.NewRequest("agent.submit", "business", input)
	next, err := s.Submit(ctx, req, input, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, middle.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := resultCaller(t, s, next, "business-process")
	child := resultChild(t, s, middle.AgentID)
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "new evidence", nil); err != nil {
		t.Fatal(err)
	}
	out, err := s.ObserveTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Mail(ctx, out.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordToolResult(ctx, middle.AgentID, next.TaskID, "request", nil, resultToolMessage(m), ToolMailDelivery{ProcessID: caller.OperationID, Messages: []string{m.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, middle.AgentID, next.TaskID, "completed", "new final", nil); err != nil {
		t.Fatal(err)
	}
	rootMail, err = s.Mailbox(ctx, protocol.MailboxQuery{AgentID: root.AgentID})
	if err != nil || len(rootMail.Items) != 2 || rootMail.Items[1].Result.TaskID != next.TaskID {
		t.Fatal(rootMail, err)
	}
}

func TestTaskResultWaitKeepsExplicitResumeAndConsumesMultipleResultsAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	_, parent := create(t, s)
	s.ClaimNext(ctx, parent.AgentID)
	caller := resultCaller(t, s, parent, "process")
	var mail []protocol.Mail
	for range 2 {
		child := resultChild(t, s, parent.AgentID)
		if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "child final", nil); err != nil {
			t.Fatal(err)
		}
		out, err := s.ObserveTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller})
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Mail(ctx, out.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		mail = append(mail, m)
	}
	mailControl(t, s, "stop", parent.AgentID)
	reserved := mailControl(t, s, "resume", parent.AgentID)
	if reserved.TaskID == parent.TaskID || reserved.TaskID == "" {
		t.Fatal(reserved)
	}
	delivery := ToolMailDelivery{ProcessID: "process", Messages: []string{mail[0].ID, mail[1].ID}}
	if err := s.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", map[string]string{"result": "observed"}, resultToolMessage(mail...), delivery); err != nil {
		t.Fatal(err)
	}
	one, _ := s.Mail(ctx, mail[0].ID)
	two, _ := s.Mail(ctx, mail[1].ID)
	if one.DeliveredSeq != two.DeliveredSeq || one.TaskID != parent.TaskID || two.TaskID != parent.TaskID {
		t.Fatal(one, two)
	}
	if err := s.FinishTask(ctx, parent.AgentID, parent.TaskID, "completed", "ignored on cancel", nil); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx, parent.AgentID)
	if len(snap.Tasks) != 2 || snap.Tasks[0].State != "cancelled" || snap.Tasks[1].ID != reserved.TaskID || snap.Tasks[1].State != "accepted" {
		t.Fatal("wait cancelled explicit resume", snap)
	}
	before, _, err := s.ModelContext(ctx, parent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := s.ClaimNext(ctx, parent.AgentID); err != nil || w == nil || w.Task.ID != reserved.TaskID {
		t.Fatal("explicit resume lost", w, err)
	}
	after, _, err := s.ModelContext(ctx, parent.AgentID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("resume duplicated result or added an empty input", err)
	}
	if err := s.FinishTask(ctx, parent.AgentID, reserved.TaskID, "completed", "continued", nil); err != nil {
		t.Fatal(err)
	}
	if w, err := s.ClaimNext(ctx, parent.AgentID); err != nil || w != nil {
		t.Fatal("extra automatic result task", w, err)
	}
}

func TestTaskResultPagingValidationCapacityAndSchemaTenMigration(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	parent := mailAgent(t, s)
	for range protocol.MaxPendingMail {
		sendMail(t, s, parent.AgentID, "queue", "existing")
	}
	child := resultChild(t, s, parent.AgentID)
	body := strings.Repeat("中文 é 🙂\n", 40000)
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", body, nil); err != nil {
		t.Fatal("capacity lost child final", err)
	}
	var got strings.Builder
	cursor := 0
	for {
		page, err := s.ReadTaskResult(ctx, protocol.ReadTaskResult{AgentID: child.AgentID, TaskID: child.TaskID, Offset: cursor, Limit: 1024})
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(page.Text)
		cursor = page.Next
		if !page.More {
			break
		}
	}
	if got.String() != body {
		t.Fatal("paged result changed")
	}
	for _, p := range []protocol.ReadTaskResult{{AgentID: child.AgentID, TaskID: child.TaskID, Offset: 1}, {AgentID: child.AgentID, TaskID: child.TaskID, Offset: len(body) + 1}, {AgentID: child.AgentID, TaskID: child.TaskID, Limit: 1}, {AgentID: parent.AgentID, TaskID: child.TaskID}} {
		if _, err := s.ReadTaskResult(ctx, p); err == nil {
			t.Fatal("invalid result cursor", p)
		}
	}
	// Build a genuine v10 database separately; migrations must not rewrite its
	// already delivered mail or task/context identities.
	s2, oldPath := newStore(t)
	a := mailAgent(t, s2)
	sent := sendMail(t, s2, a.AgentID, "queue", "before migration")
	w, err := s2.ClaimNext(ctx, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s2.Snapshot(ctx, a.AgentID)
	if _, err := s2.db.Exec(dropPluginSubscriptionSchema + `DROP TABLE plugin_progress; DROP TABLE plugin_calls; DROP TABLE mcp_calls; DROP TABLE resource_updates; DROP TABLE wait_receipts; DROP TRIGGER mail_result_identity; DROP TRIGGER mail_queue_identity;
DROP TRIGGER mail_task; DROP INDEX mail_result; DROP INDEX mail_queue_task;
DROP INDEX mail_delivery; CREATE UNIQUE INDEX mail_delivery ON mailbox(agent_id,delivered_seq) WHERE delivered_seq>0;
ALTER TABLE mailbox DROP COLUMN result_task; ALTER TABLE mailbox DROP COLUMN result_seq; ALTER TABLE mailbox DROP COLUMN queued_task;
CREATE TRIGGER mail_task BEFORE UPDATE OF task_id ON mailbox WHEN OLD.task_id IS NOT NULL OR NEW.task_id IS NULL BEGIN SELECT RAISE(ABORT,'immutable receiver task'); END;
PRAGMA user_version=10;`); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s2, err = Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, _ := s2.Snapshot(ctx, a.AgentID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration rewrote context")
	}
	m, err := s2.Mail(ctx, sent.MessageID)
	if err != nil || m.TaskID != w.Task.ID || m.State != "delivered" {
		t.Fatal(m, err)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "result_task") {
		t.Fatal(string(raw))
	}
}
