package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func skillRef(p resource.Package) protocol.ResourceRef {
	return protocol.ResourceRef{ID: p.Info.ID, Version: p.Info.Version}
}

func TestSkillInputPinsPublishedVersionAcrossQueueAndRestart(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	w := questionWork(t, s, true)
	p := savedResource(t, "/skills/review/SKILL.md", "ORIGINAL instruction")
	publication(t, s, w.Agent.ID, "first", p, 0)
	_, image := saveImage(t, s, w.Agent.ID, "image")
	in := protocol.Submit{AgentID: w.Agent.ID, Input: "review this", Attachments: []string{image.Attachment.ID}, Skills: []protocol.ResourceRef{skillRef(p)}}
	req, _ := protocol.NewRequest("agent.submit", "skill-input", in)
	accepted, err := s.Submit(ctx, req, in, 8)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := s.ResourceStatus(ctx, protocol.ResourceTarget{AgentID: w.Agent.ID, ID: p.Info.ID})
	publication(t, s, w.Agent.ID, "second", savedResource(t, p.Info.Source, "REPLACEMENT instruction"), status.Generation)
	if err := s.FinishTask(ctx, w.Agent.ID, w.Task.ID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err := s.Submit(ctx, req, in, 8)
	if err != nil || !reflect.DeepEqual(accepted, again) {
		t.Fatal(again, err)
	}
	next, err := s.ClaimNext(ctx, w.Agent.ID)
	if err != nil || next == nil || next.Task.ID != accepted.TaskID || !reflect.DeepEqual(next.Task.Skills, in.Skills) {
		t.Fatal(next, err)
	}
	messages, _, err := s.ModelContext(ctx, w.Agent.ID)
	if err != nil || len(messages) != 2 {
		t.Fatal(messages, err)
	}
	m := messages[1]
	if m.Role != "user" || len(m.Content) != 3 || m.Content[0].Text != in.Input || m.Content[1].Attachment != image.Attachment.ID || !strings.Contains(m.Content[2].Text, p.Instructions) || strings.Contains(m.Content[2].Text, "REPLACEMENT") || !strings.Contains(m.Content[2].Text, p.Info.Version) {
		t.Fatal(m)
	}
	events, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	found := 0
	for _, e := range events.Events {
		if e.TaskID == accepted.TaskID && e.Kind == "context.message" {
			var record ContextMessage
			if json.Unmarshal(e.Payload, &record) != nil || !reflect.DeepEqual(record.Resources, in.Skills) {
				t.Fatal(string(e.Payload))
			}
			found++
		}
	}
	if found != 1 {
		t.Fatal("context must be appended exactly once", found)
	}
	if _, err := s.db.Exec("UPDATE tasks SET skills='[]' WHERE id=?", accepted.TaskID); err == nil {
		t.Fatal("mutated input references")
	}
}

func TestSkillOnlyAnswerSurvivesRestartAndDeliveryRollback(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	w := questionWork(t, s, false)
	p, err := resource.BuildPackage("skill", "/skills/manual/SKILL.md", "SKILL.md", []resource.File{{Path: "SKILL.md", Data: []byte("---\nname: manual\ndescription: explicit only\ndisable-model-invocation: true\n---\nFollow the selected instructions.")}})
	if err != nil {
		t.Fatal(err)
	}
	publication(t, s, w.Agent.ID, "manual", p, 0)
	_, asked := askSaved(t, s, w, "Which instructions?")
	in := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Skills: []protocol.ResourceRef{skillRef(p)}}
	req, _ := protocol.NewRequest("question.answer", "skill-answer", in)
	accepted, err := s.AnswerQuestion(ctx, req, in)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err := s.AnswerQuestion(ctx, req, in)
	if err != nil || !reflect.DeepEqual(accepted, again) {
		t.Fatal(again, err)
	}
	before, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	if _, err := s.db.Exec("CREATE TRIGGER fail_skill_delivery BEFORE INSERT ON events WHEN NEW.kind='question.delivered' BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); ready || err == nil {
		t.Fatal("expected atomic delivery failure")
	}
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("context escaped rollback")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_skill_delivery"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if ready, err := s.QuestionBarrier(ctx, w.Agent.ID, w.Task.ID); !ready || err != nil {
			t.Fatal(ready, err)
		}
	}
	q, err := s.Question(ctx, protocol.QuestionTarget{AgentID: w.Agent.ID, ID: in.ID})
	if err != nil || q.DeliveredSeq == 0 || !reflect.DeepEqual(q.Skills, in.Skills) {
		t.Fatal(q, err)
	}
	messages, _, err := s.ModelContext(ctx, w.Agent.ID)
	if err != nil || len(messages) != 2 || messages[1].Role != "user" || !strings.Contains(messages[1].Content[len(messages[1].Content)-1].Text, p.Instructions) {
		t.Fatal(messages, err)
	}
	if _, err := s.db.Exec("UPDATE questions SET skills='[]' WHERE id=?", in.ID); err == nil {
		t.Fatal("mutated accepted answer references")
	}
}

func TestSkillInputValidationAndAcceptanceRollback(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	w := questionWork(t, s, false)
	p := savedResource(t, "/skills/review/SKILL.md", "instructions")
	publication(t, s, w.Agent.ID, "skill", p, 0)
	prompt, err := resource.BuildPackage("prompt", "/prompts/review.md", "review.md", []resource.File{{Path: "review.md", Data: []byte("---\nname: prompt\ndescription: prompt resource\n---\nPrompt body")}})
	if err != nil {
		t.Fatal(err)
	}
	publication(t, s, w.Agent.ID, "prompt", prompt, 0)
	_, asked := askSaved(t, s, w, "Choose")
	ref := skillRef(p)
	for _, refs := range [][]protocol.ResourceRef{
		{{ID: ref.ID}}, {{ID: ref.ID, Version: "latest"}},
		{{ID: ref.ID, Version: strings.Repeat("0", 64)}},
		{{ID: "foreign", Version: ref.Version}}, {ref, ref},
		make([]protocol.ResourceRef, protocol.MaxInputSkills+1), {skillRef(prompt)},
	} {
		before, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
		in := protocol.Submit{AgentID: w.Agent.ID, Skills: refs}
		req, _ := protocol.NewRequest("agent.submit", protocol.NewID("bad"), in)
		if _, err := s.Submit(ctx, req, in, 8); err == nil {
			t.Fatal("invalid skill input accepted", refs)
		}
		answer := protocol.AnswerQuestion{AgentID: w.Agent.ID, ID: asked.QuestionID, Skills: refs}
		ar, _ := protocol.NewRequest("question.answer", protocol.NewID("bad"), answer)
		if _, err := s.AnswerQuestion(ctx, ar, answer); err == nil {
			t.Fatal("invalid skill answer accepted", refs)
		}
		after, _ := s.Events(ctx, protocol.Cursor{AgentID: w.Agent.ID})
		if !reflect.DeepEqual(before, after) {
			t.Fatal("validation mutated history")
		}
	}
	other := protocol.CreateAgent{Cwd: t.TempDir()}
	or, _ := protocol.NewRequest("agent.create", "other", other)
	b, err := s.Create(ctx, or, other, 8)
	if err != nil {
		t.Fatal(err)
	}
	in := protocol.Submit{AgentID: b.AgentID, Skills: []protocol.ResourceRef{ref}}
	req, _ := protocol.NewRequest("agent.submit", "cross-agent", in)
	if _, err := s.Submit(ctx, req, in, 8); err == nil {
		t.Fatal("another Agent's skill exposed")
	}
	in.AgentID = w.Agent.ID
	req, _ = protocol.NewRequest("agent.submit", "atomic-skill", in)
	if _, err := s.db.Exec("CREATE TRIGGER fail_skill_accept BEFORE INSERT ON operations WHEN NEW.id='atomic-skill' BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx, w.Agent.ID)
	if _, err := s.Submit(ctx, req, in, 8); err == nil {
		t.Fatal("expected acceptance failure")
	}
	after, _ := s.Snapshot(ctx, w.Agent.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial skill task committed")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_skill_accept"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, req, in, 8); err != nil {
		t.Fatal("skill-only task rejected", err)
	}
}

func TestSchemaFiveMigrationPreservesInputAndResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(schema + "ALTER TABLE agents ADD COLUMN paused INTEGER NOT NULL DEFAULT 0; ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;" + attachmentSchema + questionSchema + resourceSchema)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO agents(id,cwd,state) VALUES('a','/tmp','idle'); INSERT INTO tasks(id,agent_id,state,input) VALUES('t','a','accepted','retained'); INSERT INTO questions(id,agent_id,task_id,prompt,state,answer) VALUES('q','a','t','question','answered','retained answer');")
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
	if err != nil || len(snap.Tasks) != 1 || snap.Tasks[0].Input != "retained" || len(snap.Tasks[0].Skills) != 0 {
		t.Fatal(snap, err)
	}
	q, err := s.Question(context.Background(), protocol.QuestionTarget{AgentID: "a", ID: "q"})
	if err != nil || q.Answer != "retained answer" || len(q.Skills) != 0 {
		t.Fatal(q, err)
	}
	publication(t, s, "a", "new-resource", savedResource(t, "/skills/migrated/SKILL.md", "new body"), 0)
}
