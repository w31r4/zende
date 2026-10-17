package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"

	"zende.local/harness/internal/protocol"
)

func sessionCopy(ctx context.Context, q rowQuerier, id string) (SessionCopy, error) {
	var record SessionCopy
	var raw, result, state string
	err := q.QueryRowContext(ctx, "SELECT source_agent,target_agent,cwd,plan,state,result FROM session_copies WHERE id=?", id).Scan(&record.Info.SourceAgent, &record.Info.AgentID, &record.Info.Cwd, &raw, &state, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return record, protocol.E("not_found", "session copy not found")
	}
	if err != nil {
		return record, err
	}
	var plan sessionCopyPlan
	if len(raw) > protocol.MaxFrameBytes || protocol.StrictDecode([]byte(raw), &plan) != nil || plan.Version != 1 || plan.Selection.AgentID != record.Info.SourceAgent || plan.Selection.Revision < 0 || len(plan.Drafts) > protocol.MaxCopyDrafts || !filepath.IsAbs(record.Info.Cwd) {
		return record, protocol.E("invalid_state", "retained copy intent is invalid")
	}
	record.Selection = Selection{Choice: plan.Selection, Binding: plan.Binding}
	record.Drafts = plan.Drafts
	record.Info.OperationID, record.Info.State, record.Info.Stage, record.Info.Drafts = id, state, "processes", len(plan.Drafts)
	if state == "completed" {
		var done protocol.SessionCopyInfo
		if protocol.StrictDecode([]byte(result), &done) != nil || done.OperationID != id || done.SourceAgent != record.Info.SourceAgent || done.AgentID != record.Info.AgentID || done.Cwd != record.Info.Cwd || done.State != "completed" || done.Stage != "completed" || done.SourceCursor < 0 || done.Cursor <= done.SourceCursor || done.Drafts != len(plan.Drafts) {
			return record, protocol.E("invalid_state", "completed copy receipt is invalid")
		}
		record.Info = done
		return record, nil
	}
	if state != "stopping" {
		return record, protocol.E("invalid_state", "unknown retained copy state")
	}
	stop, err := treeStop(ctx, q, id)
	if err != nil {
		return record, err
	}
	if stop.AgentID != record.Info.SourceAgent {
		return record, protocol.E("invalid_state", "copy stop scope belongs to another source")
	}
	record.Info.Stage, record.Info.Failure = stop.Stage, stop.Failure
	if record.Info.Stage == "" {
		record.Info.Stage = "processes"
	}
	if stop.State == "stopped" {
		record.Info.State, record.Info.Stage = "copying", "publish"
	}
	var progress []byte
	err = q.QueryRowContext(ctx, `SELECT payload FROM events WHERE agent_id=? AND kind='session.copy.progress'
 AND json_extract(payload,'$.operation_id')=? AND source_agent='' ORDER BY seq DESC LIMIT 1`, record.Info.SourceAgent, id).Scan(&progress)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return record, err
	}
	if err == nil {
		var latest protocol.SessionCopyInfo
		if protocol.StrictDecode(progress, &latest) != nil || latest.OperationID != id || latest.SourceAgent != record.Info.SourceAgent || latest.AgentID != record.Info.AgentID || latest.Cwd != record.Info.Cwd || latest.Drafts != record.Info.Drafts {
			return record, protocol.E("invalid_state", "copy progress identity changed")
		}
		if stop.State == "stopped" && latest.Stage == "publish" && latest.State == "copying" {
			record.Info.Failure = latest.Failure
		}
	}
	return record, nil
}

func (s *Store) SessionCopy(ctx context.Context, id string) (SessionCopy, error) {
	var record SessionCopy
	err := s.Transaction(ctx, func(tx *sql.Tx) error { var err error; record, err = sessionCopy(ctx, tx, id); return err })
	return record, err
}

func (s *Store) RecordSessionCopyFailure(ctx context.Context, id string, failure *protocol.Error) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		r, err := sessionCopy(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Info.State == "completed" || reflect.DeepEqual(r.Info.Failure, failure) {
			return nil
		}
		r.Info.Failure = failure
		_, err = appendEvent(ctx, tx, r.Info.SourceAgent, "", "session.copy.progress", r.Info)
		return err
	})
}

type PendingSessionCopy struct {
	ID     string
	Cursor int64
}

func (s *Store) PendingSessionCopies(ctx context.Context, after int64) ([]PendingSessionCopy, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,rowid FROM session_copies WHERE state='stopping' AND rowid>? ORDER BY rowid LIMIT 16", after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PendingSessionCopy{}
	for rows.Next() {
		var p PendingSessionCopy
		if err := rows.Scan(&p.ID, &p.Cursor); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
