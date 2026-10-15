package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"reflect"
	"testing"

	"zende.local/harness/internal/protocol"
)

func imageBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func imageAgent(t *testing.T, s *Store) string {
	t.Helper()
	p := protocol.CreateAgent{Cwd: t.TempDir()}
	r, _ := protocol.NewRequest("agent.create", protocol.NewID("create"), p)
	a, err := s.Create(context.Background(), r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	return a.AgentID
}

func saveImage(t *testing.T, s *Store, agent, op string) (protocol.Request, protocol.Accepted) {
	t.Helper()
	r, _ := protocol.NewRequest("attachment.import", op, protocol.ImportAttachment{AgentID: agent, Path: "image.png"})
	a, err := s.SaveImage(context.Background(), r, agent, "image.png", imageBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	return r, a
}

func TestImagesRetainBytesReferencesAndRetryAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	agent := imageAgent(t, s)
	req, accepted := saveImage(t, s, agent, "image-op")
	if accepted.Attachment == nil {
		t.Fatal("missing metadata")
	}
	ref := accepted.Attachment.ID
	p := protocol.Submit{AgentID: agent, Input: "看图\n中文", Attachments: []string{ref}}
	r, _ := protocol.NewRequest("agent.submit", "image-task", p)
	a, err := s.Submit(ctx, r, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimNext(ctx, agent)
	if err != nil || w == nil || w.Task.ID != a.TaskID || !reflect.DeepEqual(w.Task.Attachments, p.Attachments) {
		t.Fatal(w, err)
	}
	msgs, _, err := s.ModelContext(ctx, agent)
	if err != nil || len(msgs) != 1 || len(msgs[0].Content) != 2 || msgs[0].Content[1].Attachment != ref {
		t.Fatal(msgs, err)
	}
	before, err := s.Events(ctx, protocol.Cursor{AgentID: agent})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(before)
	if bytes.Contains(encoded, []byte("data:image")) || bytes.Contains(encoded, []byte(`"data":`)) {
		t.Fatal("bytes leaked into ordinary history")
	}
	journal, err := s.TaskJournal(ctx, agent, a.TaskID, nil)
	if err != nil || !reflect.DeepEqual(journal.Task.Attachments, p.Attachments) {
		t.Fatal(journal, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err := s.ReadImage(ctx, protocol.AttachmentTarget{AgentID: agent, ID: ref})
	if err != nil || !bytes.Equal(data.Data, imageBytes(t)) {
		t.Fatal("saved bytes changed", err)
	}
	// A retry must return the original acceptance before inspecting new bytes.
	req.ID = protocol.NewID("rpc")
	again, err := s.SaveImage(ctx, req, agent, "changed", []byte("invalid now"))
	if err != nil || !reflect.DeepEqual(again, accepted) {
		t.Fatal(again, err)
	}
	after, _ := s.Events(ctx, protocol.Cursor{AgentID: agent})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("retry or reads changed history")
	}
	snap, err := s.Snapshot(ctx, agent)
	if err != nil || !reflect.DeepEqual(snap.Tasks[0].Attachments, p.Attachments) {
		t.Fatal(snap, err)
	}
	req.Params = []byte(`{"agent_id":"different","path":"image.png"}`)
	if _, _, err := s.AcceptedRequest(ctx, req); err == nil {
		t.Fatal("request ID conflict accepted")
	}
	for _, statement := range []string{"UPDATE attachments SET name='changed'", "DELETE FROM attachments", "UPDATE tasks SET attachments='[]'", "UPDATE tasks SET input='replaced'"} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatal("immutable input changed", statement)
		}
	}
}

func TestImageImportTransactionRollbackAndReferenceValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	agent, other := imageAgent(t, s), imageAgent(t, s)
	_, good := saveImage(t, s, agent, "good-image")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_image BEFORE INSERT ON operations WHEN NEW.id='fail-image' BEGIN SELECT RAISE(ABORT,'injected failure'); END;`); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot(ctx, agent)
	r, _ := protocol.NewRequest("attachment.import", "fail-image", protocol.ImportAttachment{AgentID: agent, Path: "other.png"})
	if _, err := s.SaveImage(ctx, r, agent, "other.png", imageBytes(t)); err == nil {
		t.Fatal("injected commit succeeded")
	}
	after, _ := s.Snapshot(ctx, agent)
	page, err := s.Images(ctx, protocol.AttachmentList{AgentID: agent})
	if err != nil || len(page.Items) != 1 || before.Cursor != after.Cursor {
		t.Fatal("partial image commit", page, err)
	}
	for _, refs := range [][]string{{good.Attachment.ID, good.Attachment.ID}, {"missing"}, make([]string, 9)} {
		p := protocol.Submit{AgentID: agent, Attachments: refs}
		r, _ := protocol.NewRequest("agent.submit", protocol.NewID("bad"), p)
		if _, err := s.Submit(ctx, r, p, 8); err == nil {
			t.Fatal("invalid references accepted", refs)
		}
	}
	p := protocol.Submit{AgentID: other, Attachments: []string{good.Attachment.ID}}
	r, _ = protocol.NewRequest("agent.submit", "foreign", p)
	if _, err := s.Submit(ctx, r, p, 8); err == nil {
		t.Fatal("cross-Agent attachment accepted")
	}
	if _, err := s.ReadImage(ctx, protocol.AttachmentTarget{AgentID: other, ID: good.Attachment.ID}); err == nil {
		t.Fatal("cross-Agent read")
	}
	_, _ = saveImage(t, s, agent, "second-image")
	page, err = s.Images(ctx, protocol.AttachmentList{AgentID: agent, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatal(page, err)
	}
	next, err := s.Images(ctx, protocol.AttachmentList{AgentID: agent, After: page.Next, Limit: 1})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID || next.Next != "" {
		t.Fatal(next, err)
	}
}

func TestSchemaTwoMigrationKeepsExistingInputsAndPause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `ALTER TABLE agents ADD COLUMN paused INTEGER NOT NULL DEFAULT 0; ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0; PRAGMA user_version=2;
INSERT INTO agents(id,cwd,state,paused) VALUES('a','/tmp','paused',1);
INSERT INTO tasks(id,agent_id,state,input,priority) VALUES('t','a','accepted','original',3);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Snapshot(context.Background(), "a")
	if err != nil || !snap.Agent.Paused || snap.Tasks[0].Input != "original" || len(snap.Tasks[0].Attachments) != 0 {
		t.Fatal(snap, err)
	}
	var priority, version int
	s.db.QueryRow("SELECT priority FROM tasks WHERE id='t'").Scan(&priority)
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if priority != 3 || version != SchemaVersion {
		t.Fatal(priority, version)
	}
}
