package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const questionSchema = `
CREATE TABLE questions (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id),
 task_id TEXT NOT NULL REFERENCES tasks(id), prompt TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','answered','cancelled')),
 answer TEXT NOT NULL DEFAULT '', attachments TEXT NOT NULL DEFAULT '[]',
 delivered_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX question_agent ON questions(agent_id,state,id);
CREATE INDEX question_task ON questions(task_id,state,delivered_seq);
CREATE TRIGGER question_identity_no_update BEFORE UPDATE OF id,agent_id,task_id,prompt ON questions BEGIN SELECT RAISE(ABORT,'immutable question'); END;
CREATE TRIGGER question_no_delete BEFORE DELETE ON questions BEGIN SELECT RAISE(ABORT,'retained question'); END;
CREATE TRIGGER question_answer_no_update BEFORE UPDATE OF answer,attachments ON questions WHEN OLD.state!='pending' BEGIN SELECT RAISE(ABORT,'immutable answer'); END;
CREATE TRIGGER question_state_no_update BEFORE UPDATE OF state ON questions WHEN OLD.state!='pending' OR NEW.state='pending' BEGIN SELECT RAISE(ABORT,'terminal question'); END;
CREATE TRIGGER question_delivery_no_update BEFORE UPDATE OF delivered_seq ON questions WHEN OLD.state!='answered' OR OLD.delivered_seq!=0 OR NEW.delivered_seq<=0 BEGIN SELECT RAISE(ABORT,'immutable delivery'); END;
PRAGMA user_version=4;
`

const questionInfoColumns = "id,agent_id,task_id,prompt,state,delivered_seq"
const questionColumns = questionInfoColumns + ",answer,attachments,skills"

func scanQuestionInfo(row scanner) (protocol.QuestionInfo, error) {
	var q protocol.QuestionInfo
	err := row.Scan(&q.ID, &q.AgentID, &q.TaskID, &q.Prompt, &q.State, &q.DeliveredSeq)
	return q, err
}

func scanQuestion(row scanner) (protocol.Question, error) {
	var q protocol.Question
	var refs, skills string
	err := row.Scan(&q.ID, &q.AgentID, &q.TaskID, &q.Prompt, &q.State, &q.DeliveredSeq, &q.Answer, &refs, &skills)
	if errors.Is(err, sql.ErrNoRows) {
		return q, protocol.E("not_found", "question not found in this Agent")
	}
	if err == nil {
		err = json.Unmarshal([]byte(refs), &q.Attachments)
	}
	if err == nil {
		err = json.Unmarshal([]byte(skills), &q.Skills)
	}
	return q, err
}

func (s *Store) AskQuestion(ctx context.Context, req protocol.Request, p protocol.AskQuestion) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if strings.TrimSpace(p.Prompt) == "" || !utf8.ValidString(p.Prompt) || len(p.Prompt) > protocol.MaxQuestionBytes {
			return protocol.Accepted{}, protocol.E("invalid_params", "question must contain UTF-8 text up to 8 KiB")
		}
		if err := activeTask(ctx, tx, p.AgentID, p.TaskID); err != nil {
			return protocol.Accepted{}, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM questions WHERE agent_id=? AND (state='pending' OR state='answered' AND delivered_seq=0)", p.AgentID).Scan(&count); err != nil {
			return protocol.Accepted{}, err
		}
		if count >= protocol.MaxOpenQuestions {
			return protocol.Accepted{}, protocol.E("limit_exceeded", "at most 8 questions may await an answer or delivery")
		}
		q := protocol.QuestionInfo{ID: protocol.NewID("question"), AgentID: p.AgentID, TaskID: p.TaskID, Prompt: p.Prompt, State: "pending"}
		if _, err := tx.ExecContext(ctx, "INSERT INTO questions(id,agent_id,task_id,prompt,state) VALUES(?,?,?,?,?)", q.ID, q.AgentID, q.TaskID, q.Prompt, q.State); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, q.AgentID, q.TaskID, "question.asked", q); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: q.AgentID, TaskID: q.TaskID, QuestionID: q.ID}, nil
	})
}

func (s *Store) Question(ctx context.Context, p protocol.QuestionTarget) (protocol.Question, error) {
	return scanQuestion(s.db.QueryRowContext(ctx, "SELECT "+questionColumns+" FROM questions WHERE agent_id=? AND id=?", p.AgentID, p.ID))
}

func (s *Store) AnswerQuestion(ctx context.Context, req protocol.Request, p protocol.AnswerQuestion) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if !utf8.ValidString(p.Answer) || len(p.Answer) > protocol.MaxAnswerBytes || strings.TrimSpace(p.Answer) == "" && len(p.Attachments) == 0 && len(p.Skills) == 0 {
			return protocol.Accepted{}, protocol.E("invalid_params", "answer requires text, images or skills; UTF-8 text is limited to 1 MiB")
		}
		q, err := scanQuestion(tx.QueryRowContext(ctx, "SELECT "+questionColumns+" FROM questions WHERE agent_id=? AND id=?", p.AgentID, p.ID))
		if err != nil {
			return protocol.Accepted{}, err
		}
		if q.State != "pending" {
			return protocol.Accepted{}, protocol.E("conflict", "question is already answered or cancelled")
		}
		var taskState, agentState string
		var paused bool
		if err := tx.QueryRowContext(ctx, "SELECT t.state,a.state,a.paused FROM tasks t JOIN agents a ON a.id=t.agent_id WHERE t.id=? AND a.id=?", q.TaskID, q.AgentID).Scan(&taskState, &agentState, &paused); err != nil {
			return protocol.Accepted{}, err
		}
		if taskState != "running" && taskState != "unknown" || paused || agentState == "copying" || agentState == "archived" {
			return protocol.Accepted{}, protocol.E("conflict", "question task is stopped or no longer accepting answers")
		}
		if err := validateAttachments(ctx, tx, q.AgentID, p.Attachments); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := selectedSkills(ctx, tx, q.AgentID, p.Skills); err != nil {
			return protocol.Accepted{}, err
		}
		if len(p.Attachments) > 0 {
			var raw string
			err := tx.QueryRowContext(ctx, "SELECT payload FROM events WHERE agent_id=? AND task_id=? AND kind='task.config' ORDER BY seq DESC LIMIT 1", q.AgentID, q.TaskID).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return protocol.Accepted{}, err
			}
			var config struct {
				Images bool `json:"images"`
			}
			if err != nil || json.Unmarshal([]byte(raw), &config) != nil || !config.Images {
				return protocol.Accepted{}, protocol.E("unsupported_capability", "this task's model does not support images; draft attachments are retained")
			}
		}
		refs, err := json.Marshal(p.Attachments)
		if err != nil {
			return protocol.Accepted{}, err
		}
		skillRefs, err := json.Marshal(p.Skills)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET state='answered',answer=?,attachments=?,skills=? WHERE id=?", p.Answer, string(refs), string(skillRefs), q.ID); err != nil {
			return protocol.Accepted{}, err
		}
		q.State, q.Answer, q.Attachments = "answered", p.Answer, append([]string(nil), p.Attachments...)
		q.Skills = append([]protocol.ResourceRef(nil), p.Skills...)
		if _, err := appendEvent(ctx, tx, q.AgentID, q.TaskID, "question.answered", q); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: q.AgentID, TaskID: q.TaskID, QuestionID: q.ID}, nil
	})
}

func (s *Store) Questions(ctx context.Context, p protocol.QuestionList) (protocol.QuestionPage, error) {
	if p.AgentID == "" || p.Limit < 0 || p.Limit > 100 {
		return protocol.QuestionPage{}, protocol.E("invalid_params", "Agent and question limit 1–100 are required")
	}
	if p.State == "" {
		p.State = "pending"
	}
	if p.State != "pending" && p.State != "answered" && p.State != "cancelled" && p.State != "all" {
		return protocol.QuestionPage{}, protocol.E("invalid_params", "question state must be pending, answered, cancelled or all")
	}
	if p.Limit == 0 {
		p.Limit = 20
	}
	var page protocol.QuestionPage
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var found int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM agents WHERE id=?", p.AgentID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.E("not_found", "agent not found")
			}
			return err
		}
		var err error
		page, err = listQuestions(ctx, tx, p)
		return err
	})
	return page, err
}

func listQuestions(ctx context.Context, tx *sql.Tx, p protocol.QuestionList) (protocol.QuestionPage, error) {
	page := protocol.QuestionPage{Items: []protocol.QuestionInfo{}}
	rows, err := tx.QueryContext(ctx, "SELECT "+questionInfoColumns+" FROM questions WHERE agent_id=? AND id>? AND (?='all' OR state=?) ORDER BY id LIMIT ?", p.AgentID, p.After, p.State, p.State, p.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		q, err := scanQuestionInfo(rows)
		if err != nil {
			return page, err
		}
		if len(page.Items) == p.Limit {
			page.Next = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, q)
	}
	return page, rows.Err()
}

// QuestionBarrier is called after complete tool results and before a new model
// request. Answers are user messages, never tool output pretending to be one.
// Delivery and its reference commit together; replay cannot inject them twice.
func (s *Store) QuestionBarrier(ctx context.Context, agent, task string) (bool, error) {
	ready := false
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := activeTask(ctx, tx, agent, task); err != nil {
			return err
		}
		qs, err := undeliveredQuestions(ctx, tx, agent, task)
		if err != nil {
			return err
		}
		for _, q := range qs {
			if q.State == "pending" {
				return nil
			}
		}
		if err := deliverAnswers(ctx, tx, qs); err != nil {
			return err
		}
		ready = true
		return nil
	})
	return ready, err
}

func undeliveredQuestions(ctx context.Context, tx *sql.Tx, agent, task string) ([]protocol.Question, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+questionColumns+" FROM questions WHERE agent_id=? AND task_id=? AND (state='pending' OR state='answered' AND delivered_seq=0) ORDER BY rowid LIMIT ?", agent, task, protocol.MaxOpenQuestions+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var qs []protocol.Question
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		qs = append(qs, q)
	}
	if len(qs) > protocol.MaxOpenQuestions {
		return nil, protocol.E("invalid_state", "question delivery exceeds its limit")
	}
	return qs, rows.Err()
}

func deliverAnswers(ctx context.Context, tx *sql.Tx, qs []protocol.Question) error {
	for _, q := range qs {
		if q.State != "answered" {
			continue
		}
		text := fmt.Sprintf("Answer to question %s:", q.ID)
		if q.Answer != "" {
			text += "\n\n" + q.Answer
		}
		content := []model.Block{{Type: "text", Text: text}}
		for _, id := range q.Attachments {
			content = append(content, model.Block{Type: "image", Attachment: id})
		}
		skills, err := selectedSkills(ctx, tx, q.AgentID, q.Skills)
		if err != nil {
			return err
		}
		content = append(content, skills...)
		event, err := appendEvent(ctx, tx, q.AgentID, q.TaskID, "context.message", ContextMessage{QuestionID: q.ID, Resources: q.Skills, Message: model.Message{Role: "user", Content: content}})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET delivered_seq=? WHERE id=?", event.Seq, q.ID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, q.AgentID, q.TaskID, "question.delivered", map[string]any{"question_id": q.ID, "context_seq": event.Seq}); err != nil {
			return err
		}
	}
	return nil
}

func finishQuestions(ctx context.Context, tx *sql.Tx, agent, task string) error {
	qs, err := undeliveredQuestions(ctx, tx, agent, task)
	if err != nil {
		return err
	}
	for _, q := range qs {
		if q.State != "pending" {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE questions SET state='cancelled' WHERE id=?", q.ID); err != nil {
			return err
		}
		q.State = "cancelled"
		if _, err := appendEvent(ctx, tx, agent, task, "question.cancelled", q.QuestionInfo); err != nil {
			return err
		}
	}
	return deliverAnswers(ctx, tx, qs)
}
