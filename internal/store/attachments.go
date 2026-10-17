package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/attachment"
	"zende.local/harness/internal/protocol"
)

const attachmentSchema = `
ALTER TABLE tasks ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';
CREATE TABLE attachments (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), name TEXT NOT NULL,
 media_type TEXT NOT NULL, sha256 TEXT NOT NULL, bytes INTEGER NOT NULL,
 width INTEGER NOT NULL, height INTEGER NOT NULL, data BLOB NOT NULL
);
CREATE INDEX attachment_agent ON attachments(agent_id,id);
CREATE TRIGGER attachments_no_update BEFORE UPDATE ON attachments BEGIN SELECT RAISE(ABORT,'immutable attachment'); END;
CREATE TRIGGER attachments_no_delete BEFORE DELETE ON attachments BEGIN SELECT RAISE(ABORT,'retained attachment'); END;
CREATE TRIGGER task_input_no_update BEFORE UPDATE OF input,attachments ON tasks BEGIN SELECT RAISE(ABORT,'immutable task input'); END;
PRAGMA user_version=3;
`

// AcceptedRequest permits retries to return the original result before reading
// a path or checking capabilities that may have changed since acceptance.
func (s *Store) AcceptedRequest(ctx context.Context, req protocol.Request) (protocol.Accepted, bool, error) {
	if req.OperationID == "" {
		return protocol.Accepted{}, false, protocol.E("invalid_params", "operation_id is required for a mutation")
	}
	want, err := req.EffectFingerprint()
	if err != nil {
		return protocol.Accepted{}, false, err
	}
	var fingerprint, raw string
	err = s.db.QueryRowContext(ctx, "SELECT fingerprint,result FROM operations WHERE id=?", req.OperationID).Scan(&fingerprint, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Accepted{}, false, worktreeFingerprint(ctx, s.db, req.OperationID, want)
	}
	if err != nil {
		return protocol.Accepted{}, false, err
	}
	if fingerprint != want {
		return protocol.Accepted{}, false, protocol.E("operation_conflict", "operation_id was already used for a different request")
	}
	var accepted protocol.Accepted
	err = json.Unmarshal([]byte(raw), &accepted)
	return accepted, err == nil, err
}

func (s *Store) SaveImage(ctx context.Context, req protocol.Request, agent, name string, data []byte) (protocol.Accepted, error) {
	if previous, found, err := s.AcceptedRequest(ctx, req); found || err != nil {
		return previous, err
	}
	info, err := attachment.Inspect(name, data)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		if err := copyWritable(ctx, tx, agent); err != nil {
			return protocol.Accepted{}, err
		}
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM agents WHERE id=?", agent).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return protocol.Accepted{}, protocol.E("not_found", "agent not found")
			}
			return protocol.Accepted{}, err
		}
		if state == "archived" || state == "copying" {
			return protocol.Accepted{}, protocol.E("conflict", "agent is read-only")
		}
		info.ID, info.AgentID = protocol.NewID("image"), agent
		if _, err := tx.ExecContext(ctx, "INSERT INTO attachments(id,agent_id,name,media_type,sha256,bytes,width,height,data) VALUES(?,?,?,?,?,?,?,?,?)", info.ID, agent, info.Name, info.MediaType, info.SHA256, info.Bytes, info.Width, info.Height, data); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := appendEvent(ctx, tx, agent, "", "attachment.imported", info); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: agent, Attachment: &info}, nil
	})
}

const attachmentColumns = "id,agent_id,name,media_type,sha256,bytes,width,height"

type scanner interface{ Scan(...any) error }

func (s *Store) Agent(ctx context.Context, id string) (protocol.Agent, error) {
	var a protocol.Agent
	err := s.db.QueryRowContext(ctx, "SELECT id,coalesce(parent_id,''),cwd,state,model,seq,paused FROM agents WHERE id=?", id).Scan(&a.ID, &a.ParentID, &a.Cwd, &a.State, &a.Model, &a.Seq, &a.Paused)
	if errors.Is(err, sql.ErrNoRows) {
		err = protocol.E("not_found", "agent not found")
	}
	return a, err
}

func scanAttachment(row scanner) (protocol.Attachment, error) {
	var a protocol.Attachment
	err := row.Scan(&a.ID, &a.AgentID, &a.Name, &a.MediaType, &a.SHA256, &a.Bytes, &a.Width, &a.Height)
	if errors.Is(err, sql.ErrNoRows) {
		err = protocol.E("not_found", "attachment not found in this Agent")
	}
	return a, err
}

func (s *Store) ImageInfo(ctx context.Context, target protocol.AttachmentTarget) (protocol.Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx, "SELECT "+attachmentColumns+" FROM attachments WHERE agent_id=? AND id=?", target.AgentID, target.ID))
}

func (s *Store) ReadImage(ctx context.Context, target protocol.AttachmentTarget) (protocol.AttachmentData, error) {
	info, err := s.ImageInfo(ctx, target)
	if err != nil {
		return protocol.AttachmentData{}, err
	}
	if info.Bytes < 1 || info.Bytes > protocol.MaxImageBytes {
		return protocol.AttachmentData{}, protocol.E("attachment_invalid", "saved attachment exceeds the byte limit")
	}
	var data []byte
	err = s.db.QueryRowContext(ctx, "SELECT CASE WHEN length(data)<=? THEN data ELSE NULL END FROM attachments WHERE agent_id=? AND id=?", protocol.MaxImageBytes, target.AgentID, target.ID).Scan(&data)
	if err != nil {
		return protocol.AttachmentData{}, err
	}
	digest := sha256.Sum256(data)
	if len(data) != info.Bytes || hex.EncodeToString(digest[:]) != info.SHA256 {
		return protocol.AttachmentData{}, protocol.E("attachment_invalid", "saved attachment failed integrity verification")
	}
	return protocol.AttachmentData{Attachment: info, Data: data}, nil
}

func (s *Store) Images(ctx context.Context, p protocol.AttachmentList) (protocol.AttachmentPage, error) {
	if p.AgentID == "" || p.Limit < 0 || p.Limit > 100 {
		return protocol.AttachmentPage{}, protocol.E("invalid_params", "Agent and attachment limit 1–100 are required")
	}
	if p.Limit == 0 {
		p.Limit = 20
	}
	if _, err := s.Agent(ctx, p.AgentID); err != nil {
		return protocol.AttachmentPage{}, err
	}
	page := protocol.AttachmentPage{Items: []protocol.Attachment{}}
	rows, err := s.db.QueryContext(ctx, "SELECT "+attachmentColumns+" FROM attachments WHERE agent_id=? AND id>? ORDER BY id LIMIT ?", p.AgentID, p.After, p.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		info, err := scanAttachment(rows)
		if err != nil {
			return protocol.AttachmentPage{}, err
		}
		if len(page.Items) == p.Limit {
			page.Next = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, info)
	}
	return page, rows.Err()
}

func validateAttachments(ctx context.Context, tx *sql.Tx, agent string, refs []string) error {
	if len(refs) > protocol.MaxInputImages {
		return protocol.E("attachment_limit", "input supports up to 8 images")
	}
	total, seen := 0, make(map[string]bool)
	for _, id := range refs {
		if id == "" || seen[id] {
			return protocol.E("invalid_params", "attachment IDs must be nonempty and unique")
		}
		seen[id] = true
		info, err := scanAttachment(tx.QueryRowContext(ctx, "SELECT "+attachmentColumns+" FROM attachments WHERE agent_id=? AND id=?", agent, id))
		if err != nil {
			return err
		}
		total += info.Bytes
		if info.Bytes < 1 || info.Bytes > protocol.MaxImageBytes || total > protocol.MaxRequestImageBytes {
			return protocol.E("attachment_limit", "image input exceeds the byte budget")
		}
	}
	return nil
}

func scanTask(row scanner) (protocol.Task, error) {
	var t protocol.Task
	var raw, skills string
	if err := row.Scan(&t.ID, &t.AgentID, &t.State, &t.Input, &t.Result, &raw, &skills); err != nil {
		return t, err
	}
	if err := json.Unmarshal([]byte(raw), &t.Attachments); err != nil {
		return t, err
	}
	return t, json.Unmarshal([]byte(skills), &t.Skills)
}
