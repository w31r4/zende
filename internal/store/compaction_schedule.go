package store

import (
	"context"
	"database/sql"
	"errors"

	"zende.local/harness/internal/protocol"
)

// ActiveCompaction returns the one accepted operation for this Agent. No
// operation is a normal result; an unreadable projection is not.
func (s *Store) ActiveCompaction(ctx context.Context, agent string) (*CompactionRecord, error) {
	var result *CompactionRecord
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM compactions WHERE agent_id=? AND json_extract(record,'$.state') IN ('pending','running')`, agent).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := compactionRecord(ctx, tx, id)
		if err == nil {
			result = &r
		}
		return err
	})
	return result, err
}

func manualCompactionBoundary(ctx context.Context, tx *sql.Tx, agent string) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM compactions WHERE agent_id=? AND json_extract(record,'$.reason')='manual' AND json_extract(record,'$.state')='pending')`, agent).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return protocol.E("request_conflict", "manual compaction arrived before request start; handle it at this boundary")
	}
	return nil
}

// Only idle/paused Agents may run a standalone summary. Running and unknown
// Tasks keep their own worker and recovery gate, including outstanding tools.
func (s *Store) IdleCompactions(ctx context.Context, limit int) ([]protocol.CompactionInfo, error) {
	if limit < 1 {
		return nil, protocol.E("invalid_params", "compaction scheduler requires a positive worker limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.agent_id FROM compactions c JOIN agents a ON a.id=c.agent_id
WHERE a.state IN ('idle','paused') AND json_extract(c.record,'$.reason')='manual'
AND json_extract(c.record,'$.state')='pending' ORDER BY c.rowid LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []protocol.CompactionInfo
	for rows.Next() {
		var item protocol.CompactionInfo
		if err := rows.Scan(&item.OperationID, &item.AgentID); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}
