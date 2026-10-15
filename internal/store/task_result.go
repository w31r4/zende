package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const taskResultSchema = `
ALTER TABLE mailbox ADD COLUMN result_task TEXT REFERENCES tasks(id);
ALTER TABLE mailbox ADD COLUMN result_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mailbox ADD COLUMN queued_task TEXT REFERENCES tasks(id);
CREATE UNIQUE INDEX mail_result ON mailbox(result_task) WHERE result_task IS NOT NULL;
CREATE UNIQUE INDEX mail_queue_task ON mailbox(queued_task) WHERE queued_task IS NOT NULL;
DROP INDEX mail_delivery;
CREATE INDEX mail_delivery ON mailbox(agent_id,delivered_seq) WHERE delivered_seq>0;
CREATE TRIGGER mail_result_identity BEFORE UPDATE OF result_task,result_seq ON mailbox
 BEGIN SELECT RAISE(ABORT,'immutable task result'); END;
CREATE TRIGGER mail_queue_identity BEFORE UPDATE OF queued_task ON mailbox WHEN OLD.queued_task IS NOT NULL OR NEW.queued_task IS NULL
 BEGIN SELECT RAISE(ABORT,'immutable queue task'); END;
DROP TRIGGER mail_task;
CREATE TRIGGER mail_task BEFORE UPDATE OF task_id ON mailbox WHEN NEW.task_id IS NULL OR OLD.delivered_seq>0 OR
 (OLD.task_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM tasks WHERE id=OLD.task_id AND state='accepted' AND id=OLD.queued_task))
 BEGIN SELECT RAISE(ABORT,'immutable delivered task'); END;
CREATE TABLE wait_receipts (
 process_id TEXT NOT NULL, message_id TEXT NOT NULL REFERENCES mailbox(id), source TEXT NOT NULL,
 PRIMARY KEY(process_id,message_id)
);
CREATE TRIGGER wait_identity BEFORE UPDATE ON wait_receipts BEGIN SELECT RAISE(ABORT,'immutable wait receipt'); END;
CREATE TRIGGER wait_no_delete BEFORE DELETE ON wait_receipts BEGIN SELECT RAISE(ABORT,'retained wait receipt'); END;
PRAGMA user_version=11;
`

func returnTaskResult(ctx context.Context, tx *sql.Tx, agent, task, result string, seq int64) error {
	var parent string
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(parent_id,'') FROM agents WHERE id=?", agent).Scan(&parent); err != nil {
		return err
	}
	if parent == "" {
		return nil
	}
	var notification bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailbox WHERE queued_task=? AND result_task IS NOT NULL)", task).Scan(&notification); err != nil {
		return err
	}
	if notification {
		return nil
	} // Processing a result never creates an automatic acknowledgement.
	if len(result) > protocol.MaxTaskResultBytes || !utf8.ValidString(result) {
		return protocol.E("invalid_state", "task final exceeds the supported result budget")
	}
	var receiverSeq, senderSeq int64
	if err := tx.QueryRowContext(ctx, "SELECT seq+1 FROM agents WHERE id=?", parent).Scan(&receiverSeq); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT seq+1 FROM agents WHERE id=?", agent).Scan(&senderSeq); err != nil {
		return err
	}
	m := protocol.Mail{MailInfo: protocol.MailInfo{ID: protocol.NewID("message"), AgentID: parent, OperationID: protocol.NewID("result"), SenderKind: "agent", Delivery: "queue", Seq: receiverSeq, SentSeq: senderSeq, Bytes: len(result), State: "accepted",
		Result: &protocol.TaskResultRef{AgentID: agent, TaskID: task, Seq: seq}}, Input: result}
	// Completion already owes its parent this result. User-input capacity must
	// not discard a finished child's output or prevent its terminal commit.
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox(id,agent_id,operation_id,source,sender_id,delivery,seq,sent_seq,input,result_task,result_seq)
 VALUES(?,?,?,'null',?,'queue',?,?,?,?,?)`, m.ID, parent, m.OperationID, agent, m.Seq, m.SentSeq, result, task, seq); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, parent, "", "mailbox.accepted", m); err != nil {
		return err
	}
	_, err := appendEvent(ctx, tx, agent, task, "mailbox.sent", m.MailInfo)
	return err
}

func (s *Store) ObserveTask(ctx context.Context, p protocol.WaitTask) (protocol.TaskOutcome, error) {
	out := protocol.TaskOutcome{AgentID: p.AgentID, TaskID: p.TaskID}
	if p.AgentID == "" || p.TaskID == "" || p.WaitMS < 0 || p.WaitMS > protocol.MaxAgentWaitMS {
		return out, protocol.E("invalid_params", "wait requires agent, task and wait_ms 0..30000")
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var result string
		if err := tx.QueryRowContext(ctx, "SELECT state,result FROM tasks WHERE id=? AND agent_id=?", p.TaskID, p.AgentID).Scan(&out.State, &result); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("not_found", "target task not found")
			}
			return err
		}
		source, err := callSource(ctx, tx, p.Caller)
		if err != nil {
			return err
		}
		out.Terminal = out.State == "completed" || out.State == "failed" || out.State == "cancelled"
		if !out.Terminal {
			return nil
		}
		var seq int64
		var raw []byte
		if err := tx.QueryRowContext(ctx, "SELECT seq,payload FROM events WHERE agent_id=? AND task_id=? AND kind=? ORDER BY seq DESC LIMIT 1", p.AgentID, p.TaskID, "task."+out.State).Scan(&seq, &raw); err != nil {
			return err
		}
		var terminal struct {
			Error *protocol.Error `json:"error"`
		}
		if err := json.Unmarshal(raw, &terminal); err != nil {
			return err
		}
		out.Error = terminal.Error
		if out.State != "completed" {
			return nil
		}
		out.Result = &protocol.TaskResultRef{AgentID: p.AgentID, TaskID: p.TaskID, Seq: seq}
		out.Bytes = len(result)
		var recipient string
		err = tx.QueryRowContext(ctx, "SELECT id,agent_id FROM mailbox WHERE result_task=?", p.TaskID).Scan(&out.MessageID, &recipient)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if source != nil && source.Caller.AgentID == recipient {
			raw, _ := json.Marshal(source)
			_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO wait_receipts(process_id,message_id,source) VALUES(?,?,?)", source.Caller.OperationID, out.MessageID, string(raw))
			return err
		}
		return nil
	})
	return out, err
}

func (s *Store) ReadTaskResult(ctx context.Context, p protocol.ReadTaskResult) (protocol.TaskResultPage, error) {
	var out protocol.TaskResultPage
	if p.AgentID == "" || p.TaskID == "" || p.Offset < 0 || p.Limit < 0 || p.Limit > protocol.MaxResultPageBytes {
		return out, protocol.E("invalid_params", "result requires agent, task, nonnegative byte offset and limit up to 65536")
	}
	if p.Limit == 0 {
		p.Limit = protocol.MaxResultPageBytes
	}
	var state, text string
	err := s.db.QueryRowContext(ctx, `SELECT t.state,t.result,e.seq FROM tasks t JOIN events e ON e.agent_id=t.agent_id AND e.task_id=t.id AND e.kind='task.completed' WHERE t.agent_id=? AND t.id=?`, p.AgentID, p.TaskID).Scan(&state, &text, &out.Ref.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return out, protocol.E("not_found", "completed task result not found")
	}
	if err != nil {
		return out, err
	}
	if state != "completed" || !utf8.ValidString(text) {
		return out, protocol.E("invalid_state", "task result is not complete UTF-8")
	}
	if p.Offset > len(text) || (p.Offset < len(text) && !utf8.RuneStart(text[p.Offset])) {
		return out, protocol.E("invalid_cursor", "result offset is not a UTF-8 byte boundary")
	}
	end := min(len(text), p.Offset+p.Limit)
	for end > p.Offset && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == p.Offset && end < len(text) {
		return out, protocol.E("limit_exceeded", "result page limit cannot fit the next UTF-8 character")
	}
	out.Ref.AgentID, out.Ref.TaskID = p.AgentID, p.TaskID
	out.Text, out.Next, out.Total, out.More = text[p.Offset:end], end, len(text), end < len(text)
	return out, nil
}

// Wait receipts are observation metadata. Only a subsequent, durable tool
// context carrying the full result can consume its automatic queue message.
func (s *Store) WaitedResults(ctx context.Context, agent, process string) ([]protocol.Mail, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+mailColumns+",m.input FROM wait_receipts w JOIN mailbox m ON m.id=w.message_id LEFT JOIN tasks t ON t.id=m.task_id WHERE w.process_id=? AND m.agent_id=? AND m.delivered_seq=0 ORDER BY m.seq LIMIT 128", process, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mail []protocol.Mail
	for rows.Next() {
		m, err := scanMail(rows, true)
		if err != nil {
			return nil, err
		}
		mail = append(mail, m)
	}
	return mail, rows.Err()
}

type ToolMailDelivery struct {
	ProcessID string
	Messages  []string
}

func consumeWaitResults(ctx context.Context, tx *sql.Tx, agent, task string, seq int64, message model.Message, delivery ToolMailDelivery) error {
	seen := map[string]bool{}
	for _, id := range delivery.Messages {
		if seen[id] {
			return protocol.E("invalid_state", "duplicate wait result delivery")
		}
		seen[id] = true
		m, err := scanMail(tx.QueryRowContext(ctx, "SELECT "+mailColumns+",m.input FROM mailbox m LEFT JOIN tasks t ON t.id=m.task_id JOIN wait_receipts w ON w.message_id=m.id WHERE m.id=? AND m.agent_id=? AND w.process_id=?", id, agent, delivery.ProcessID), true)
		if err != nil {
			return err
		}
		if m.DeliveredSeq > 0 {
			return protocol.E("conflict", "wait result already entered context")
		}
		if m.Result == nil || message.Role != "tool" {
			return protocol.E("invalid_state", "wait delivery requires a task result in tool context")
		}
		present := false
		for _, block := range message.Content {
			if block.Type == "text" && block.CallID != "" && block.Text == protocol.MailResultText(m) {
				present = true
				break
			}
		}
		if !present {
			return protocol.E("invalid_state", "wait result body is absent from tool context")
		}
		if m.TaskID != "" && m.TaskID != task {
			// Resume may have reserved a task before an in-flight tool settled.
			// Move delivery to its actual context task. Keep the user's accepted
			// resume task: it continues that saved context without redelivery.
			var reserved bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE id=? AND state='accepted' AND id=(SELECT queued_task FROM mailbox WHERE id=?))", m.TaskID, id).Scan(&reserved); err != nil {
				return err
			}
			if !reserved {
				return protocol.E("conflict", "queued result already started")
			}
			if _, err := tx.ExecContext(ctx, "UPDATE mailbox SET task_id=? WHERE id=?", task, id); err != nil {
				return err
			}
		} else if m.TaskID == "" {
			if _, err := tx.ExecContext(ctx, "UPDATE mailbox SET task_id=? WHERE id=?", task, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mailbox SET delivered_seq=? WHERE id=?", seq, id); err != nil {
			return err
		}
		m.TaskID, m.DeliveredSeq, m.State = task, seq, "delivered"
		if err := tx.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id=?", task).Scan(&m.TaskState); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, agent, task, "mailbox.delivered", m.MailInfo); err != nil {
			return err
		}
	}
	return nil
}
