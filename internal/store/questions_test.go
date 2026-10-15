package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func questionWork(t *testing.T, s *Store, images bool) Work {
	t.Helper()
	_, a := create(t, s)
	w, err := s.ClaimNext(context.Background(), a.AgentID)
	if err != nil || w == nil {
		t.Fatal(w, err)
	}
	if err := s.AppendTaskEvent(context.Background(), a.AgentID, w.Task.ID, "task.config", map[string]bool{"images": images}); err != nil {
		t.Fatal(err)
	}
	return *w
}

func askSaved(t *testing.T, s *Store, w Work, prompt string) (protocol.Request, protocol.Accepted) {
	t.Helper()
	p := protocol.AskQuestion{AgentID: w.Agent.ID, TaskID: w.Task.ID, Prompt: prompt}
	r, _ := protocol.NewRequest("question.ask", protocol.NewID("ask"), p)
	a, err := s.AskQuestion(context.Background(), r, p)
	if err != nil || a.QuestionID == "" {
		t.Fatal(a, err)
	}
	return r, a
}

func TestQuestionAnswerDeliveryIsDurableOrderedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	w := questionWork(t, s, true)
	askReq, asked := askSaved(t, s, w, "看哪个文件？")
	var ask protocol.AskQuestion
	json.Unmarshal(askReq.Params, &ask)
	again, err := s.AskQuestion(ctx, askReq, ask)
	if err != nil || !reflect.DeepEqual(again, asked) {
		t.Fatal("ask retry", again, err)
	}
	snap, err := s.Snapshot(ctx, w.Agent.ID)
	if err != nil || len(snap.Questions) != 1 || snap.Questions[0].ID != asked.QuestionID {
		t.Fatal(snap, err)
	}
	_, saved := saveImage(t, s, w.Agent.ID, "image-for-answer")
	p := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Answer: "中文\n第二行", Attachments: []string{saved.Attachment.ID}}
	r, _ := protocol.NewRequest("question.answer", "answer", p)
	accepted, err := s.AnswerQuestion(ctx, r, p)
	if err != nil || accepted.QuestionID != asked.QuestionID || accepted.TaskID != w.Task.ID {
		t.Fatal(accepted, err)
	}
	msgs, _, err := s.ModelContext(ctx, w.Agent.ID)
	if err != nil || len(msgs) != 1 {
		t.Fatal("answer injected inside a tool call", msgs, err)
	}
	// The complete tool result precedes the user answer at the request boundary.
	if err := s.RecordToolResult(ctx, w.Agent.ID, w.Task.ID, "request", struct{}{}, model.Message{Role: "tool", Content: []model.Block{{Type: "text", Text: "question accepted"}}}); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
	q, err := s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: asked.QuestionID})
	if err != nil || q.DeliveredSeq == 0 || q.State != "answered" || q.Answer != p.Answer || !reflect.DeepEqual(q.Attachments, p.Attachments) {
		t.Fatal(q, err)
	}
	msgs, _, err = s.ModelContext(ctx, w.Agent.ID)
	if err != nil || len(msgs) != 3 || msgs[1].Role != "tool" || msgs[2].Role != "user" || !strings.Contains(msgs[2].Content[0].Text, p.Answer) || msgs[2].Content[1].Attachment != saved.Attachment.ID {
		t.Fatal("answer order or refs", msgs, err)
	}
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
	again, err = s.AnswerQuestion(ctx, r, p)
	if err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal("answer retry after restart", err)
	}
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("delivery or acceptance repeated")
	}
	page, err := s.Questions(ctx, protocol.QuestionList{AgentID: w.Agent.ID, State: "answered"})
	encoded, _ := json.Marshal(page)
	if err != nil || len(page.Items) != 1 || strings.Contains(string(encoded), p.Answer) || strings.Contains(string(encoded), "attachments") {
		t.Fatal("list expands answers", page, err)
	}
	for _, statement := range []string{"UPDATE questions SET prompt='changed'", "UPDATE questions SET answer='changed'", "UPDATE questions SET state='pending'", "UPDATE questions SET delivered_seq=1", "DELETE FROM questions"} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatal("immutable question changed", statement)
		}
	}
}

func TestQuestionTransactionsRollBackAcceptanceAndDelivery(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	w := questionWork(t, s, false)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_accept BEFORE INSERT ON operations BEGIN SELECT RAISE(ABORT,'injected acceptance failure'); END;`); err != nil {
		t.Fatal(err)
	}
	p := protocol.AskQuestion{AgentID: w.Agent.ID, TaskID: w.Task.ID, Prompt: "Question"}
	r, _ := protocol.NewRequest("question.ask", "ask", p)
	if _, err := s.AskQuestion(ctx, r, p); err == nil {
		t.Fatal("expected failure")
	}
	snap, _ := s.Snapshot(ctx, w.Agent.ID)
	if len(snap.Questions) != 0 {
		t.Fatal("rolled back ask retained")
	}
	s.db.Exec("DROP TRIGGER fail_accept")
	asked, err := s.AskQuestion(ctx, r, p)
	if err != nil {
		t.Fatal(err)
	}
	answer := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Answer: "Yes"}
	ar, _ := protocol.NewRequest("question.answer", "answer", answer)
	s.db.Exec(`CREATE TRIGGER fail_answer BEFORE INSERT ON operations WHEN NEW.id='answer' BEGIN SELECT RAISE(ABORT,'injected answer failure'); END;`)
	if _, err := s.AnswerQuestion(ctx, ar, answer); err == nil {
		t.Fatal("expected answer failure")
	}
	q, _ := s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: asked.QuestionID})
	if q.State != "pending" || q.Answer != "" {
		t.Fatal("answer escaped rollback", q)
	}
	s.db.Exec("DROP TRIGGER fail_answer")
	if _, err := s.AnswerQuestion(ctx, ar, answer); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	s.db.Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON events WHEN NEW.kind='question.delivered' BEGIN SELECT RAISE(ABORT,'injected delivery failure'); END;`)
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err == nil || ready {
		t.Fatal("expected delivery failure")
	}
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	q, _ = s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: asked.QuestionID})
	if !reflect.DeepEqual(before, after) || q.DeliveredSeq != 0 {
		t.Fatal("partial delivery committed")
	}
	s.db.Exec("DROP TRIGGER fail_delivery")
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
}

func TestQuestionsBoundInputScopeAndWaitForAllAnswers(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	w := questionWork(t, s, false)
	for _, prompt := range []string{"", " \n ", strings.Repeat("a", protocol.MaxQuestionBytes+1), string([]byte{255})} {
		p := protocol.AskQuestion{AgentID: w.Agent.ID, TaskID: w.Task.ID, Prompt: prompt}
		r, _ := protocol.NewRequest("question.ask", protocol.NewID("bad"), p)
		if _, err := s.AskQuestion(ctx, r, p); err == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
	var ids []string
	for range protocol.MaxOpenQuestions {
		_, a := askSaved(t, s, w, "Question")
		ids = append(ids, a.QuestionID)
	}
	p := protocol.AskQuestion{AgentID: w.Agent.ID, TaskID: w.Task.ID, Prompt: "Overflow"}
	r, _ := protocol.NewRequest("question.ask", "overflow", p)
	if _, err := s.AskQuestion(ctx, r, p); err == nil {
		t.Fatal("question limit bypassed")
	}
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err != nil || ready {
		t.Fatal("did not wait for answers", ready, err)
	}
	if _, err := s.Question(ctx, protocol.QuestionTarget{AgentID: "another", ID: ids[0]}); err == nil {
		t.Fatal("cross-Agent read")
	}
	_, image := saveImage(t, s, w.Agent.ID, "image")
	for _, answer := range []protocol.AnswerQuestion{
		{AgentID: w.Agent.ID, ID: ids[0]},
		{AgentID: w.Agent.ID, ID: ids[0], Answer: strings.Repeat("a", protocol.MaxAnswerBytes+1)},
		{AgentID: w.Agent.ID, ID: ids[0], Answer: "image", Attachments: []string{image.Attachment.ID}},
		{AgentID: "another", ID: ids[0], Answer: "wrong owner"},
	} {
		r, _ := protocol.NewRequest("question.answer", protocol.NewID("bad"), answer)
		if _, err := s.AnswerQuestion(ctx, r, answer); err == nil {
			t.Fatal("invalid answer accepted")
		}
	}
	for i, id := range ids {
		p := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: id, Answer: fmt.Sprint(i)}
		r, _ := protocol.NewRequest("question.answer", protocol.NewID("answer"), p)
		if _, err := s.AnswerQuestion(ctx, r, p); err != nil {
			t.Fatal(err)
		}
		ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID)
		if err != nil || ready != (i == len(ids)-1) {
			t.Fatal("partial batch advanced", i, ready, err)
		}
	}
	msgs, _, _ := s.ModelContext(ctx, w.Agent.ID)
	if len(msgs) != len(ids)+1 {
		t.Fatal("missing answers", len(msgs))
	}
	page, err := s.Questions(ctx, protocol.QuestionList{AgentID: w.Agent.ID, State: "all", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatal(page, err)
	}
	next, err := s.Questions(ctx, protocol.QuestionList{AgentID: w.Agent.ID, State: "all", Limit: 1, After: page.Next})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatal(next, err)
	}
}

func TestQuestionConcurrentAnswerAndTaskStopKeepHistory(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	w := questionWork(t, s, false)
	_, asked := askSaved(t, s, w, "Choose")
	p := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Answer: "Yes"}
	r, _ := protocol.NewRequest("question.answer", "one-answer", p)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a, err := s.AnswerQuestion(ctx, r, p); err != nil || a.QuestionID != asked.QuestionID {
				t.Error(a, err)
			}
		}()
	}
	wg.Wait()
	_, pending := askSaved(t, s, w, "Still waiting")
	stop := protocol.Target{AgentID: w.Agent.ID}
	sr, _ := protocol.NewRequest("agent.stop", "stop", stop)
	if _, err := s.Stop(ctx, sr, stop); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); err == nil || ready {
		t.Fatal("paused task advanced")
	}
	if err := s.FinishTask(ctx, w.Agent.ID, w.Task.ID, "completed", "", nil); err != nil {
		t.Fatal(err)
	}
	q, err := s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: pending.QuestionID})
	if err != nil || q.State != "cancelled" {
		t.Fatal("pending question survived finished Task", q, err)
	}
	p.ID = pending.QuestionID
	r, _ = protocol.NewRequest("question.answer", "too-late", p)
	if _, err := s.AnswerQuestion(ctx, r, p); err == nil {
		t.Fatal("answered cancelled question")
	}
	q, err = s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: asked.QuestionID})
	if err != nil || q.DeliveredSeq == 0 || q.Answer != "Yes" {
		t.Fatal("accepted answer lost at stop", q, err)
	}
	snap, _ := s.Snapshot(ctx, w.Agent.ID)
	if len(snap.Questions) != 0 || !snap.Agent.Paused || snap.Tasks[0].State != "cancelled" {
		t.Fatal(snap)
	}
}

func TestSchemaThreeMigrationKeepsImagesAndTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(schema + `ALTER TABLE agents ADD COLUMN paused INTEGER NOT NULL DEFAULT 0; ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;` + attachmentSchema + `
INSERT INTO agents(id,cwd,state) VALUES('a','/tmp','idle');
INSERT INTO tasks(id,agent_id,state,input,attachments) VALUES('t','a','accepted','original','["image"]');`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO attachments(id,agent_id,name,media_type,sha256,bytes,width,height,data) VALUES('image','a','original.png','image/png','digest',3,1,1,?)", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot(context.Background(), "a")
	if err != nil || len(snap.Tasks) != 1 || snap.Tasks[0].Input != "original" || !reflect.DeepEqual(snap.Tasks[0].Attachments, []string{"image"}) || len(snap.Questions) != 0 {
		t.Fatal(snap, err)
	}
	var data []byte
	if err := s.db.QueryRow("SELECT data FROM attachments WHERE id='image'").Scan(&data); err != nil || !reflect.DeepEqual(data, []byte{1, 2, 3}) {
		t.Fatal(data, err)
	}
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
}
