package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/protocol"
)

// All reads/writes use the publication transaction. A failure leaves no target
// rows, image mappings or partially published history behind.
func copySessionImages(ctx context.Context, tx *sql.Tx, copyID, source, target string) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM attachments WHERE agent_id=? AND id>? ORDER BY id LIMIT 64", source, after)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			mapped := protocol.NewID("image")
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,agent_id,name,media_type,sha256,bytes,width,height,data)
 SELECT ?,?,name,media_type,sha256,bytes,width,height,data FROM attachments WHERE agent_id=? AND id=?`, mapped, target, source, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO session_copy_images(copy_id,source_id,target_id) VALUES(?,?,?)", copyID, id, mapped); err != nil {
				return err
			}
			after = id
		}
	}
}

func copiedImage(ctx context.Context, tx *sql.Tx, copyID, old string) (string, error) {
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT target_id FROM session_copy_images WHERE copy_id=? AND source_id=?", copyID, old).Scan(&id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		return "", protocol.E("context_invalid", "copied input refers to a missing source image")
	}
	return id, nil
}

func copiedImageRefs(ctx context.Context, tx *sql.Tx, copyID string, original []string) ([]string, error) {
	refs := make([]string, 0, len(original))
	for _, id := range original {
		mapped, err := copiedImage(ctx, tx, copyID, id)
		if err != nil {
			return nil, err
		}
		refs = append(refs, mapped)
	}
	return refs, nil
}

type copiedHistoryEvent struct {
	seq                 int64
	kind, task, payload string
	source              protocol.CopiedEventSource
}

// Keep sequence numbers so raw context ranges and view boundaries remain valid.
// Each bounded page is closed before inserts on the single SQLite connection.
func copySessionHistory(ctx context.Context, tx *sql.Tx, copyID, source, target string, cursor int64) error {
	var after int64
	for after < cursor {
		rows, err := tx.QueryContext(ctx, "SELECT seq,kind,task_id,payload,source_agent,source_seq FROM events WHERE agent_id=? AND seq>? AND seq<=? ORDER BY seq LIMIT 32", source, after, cursor)
		if err != nil {
			return err
		}
		var batch []copiedHistoryEvent
		bytes := 0
		for rows.Next() {
			var e copiedHistoryEvent
			if err := rows.Scan(&e.seq, &e.kind, &e.task, &e.payload, &e.source.AgentID, &e.source.Seq); err != nil {
				rows.Close()
				return err
			}
			if len(e.payload) > protocol.MaxFrameBytes {
				rows.Close()
				return protocol.E("output_limit", "source history event exceeds the retained frame budget")
			}
			if len(batch) > 0 && bytes+len(e.payload) > protocol.MaxFrameBytes {
				break
			}
			bytes += len(e.payload)
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return protocol.E("cursor_gap", "source history range is incomplete; source is retained")
		}
		for _, e := range batch {
			if e.seq != after+1 {
				return protocol.E("cursor_gap", "source history range is incomplete; source is retained")
			}
			if e.source.AgentID == "" {
				if e.source.Seq != 0 {
					return protocol.E("invalid_state", "source history provenance is invalid")
				}
				e.source = protocol.CopiedEventSource{AgentID: source, Seq: e.seq}
			}
			if e.source.Seq <= 0 {
				return protocol.E("invalid_state", "source history provenance is invalid")
			}
			payload := e.payload
			if e.kind == "context.message" {
				var message ContextMessage
				if protocol.StrictDecode([]byte(payload), &message) != nil {
					return protocol.E("context_invalid", "source context message is invalid")
				}
				changed := false
				for i := range message.Message.Content {
					b := &message.Message.Content[i]
					if b.Type == "image" {
						mapped, err := copiedImage(ctx, tx, copyID, b.Attachment)
						if err != nil {
							return err
						}
						b.Attachment = mapped
						changed = true
					}
				}
				if changed {
					raw, err := json.Marshal(message)
					if err != nil {
						return err
					}
					payload = string(raw)
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO events(agent_id,seq,kind,task_id,payload,source_agent,source_seq) VALUES(?,?,?,?,?,?,?)", target, e.seq, e.kind, e.task, payload, e.source.AgentID, e.source.Seq); err != nil {
				return err
			}
			after = e.seq
		}
	}
	_, err := tx.ExecContext(ctx, "UPDATE agents SET seq=? WHERE id=? AND state='copying'", cursor, target)
	return err
}

func copySessionResources(ctx context.Context, tx *sql.Tx, source, target, selectedPlugin string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO resource_publications(agent_id,generation,id,version)
 SELECT ?,generation,id,version FROM resource_publications WHERE agent_id=?`, target, source); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_resources(agent_id,id,version,generation)
 SELECT ?,id,version,generation FROM agent_resources WHERE agent_id=? AND id<>?`, target, source, selectedPlugin); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO resource_updates(agent_id,id,record)
 SELECT ?,id,record FROM resource_updates WHERE agent_id=? AND id<>? AND json_extract(record,'$.update.state')<>'preparing'`, target, source, selectedPlugin)
	return err
}
