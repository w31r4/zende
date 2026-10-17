package store

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

// Newest-created first, using the retained Agent identity as the keyset cursor.
// New sessions cannot shift a previously returned page. No history is removed.
func (s *Store) AgentPage(ctx context.Context, p protocol.AgentPageQuery) (protocol.AgentPage, error) {
	if p.Limit < 0 || p.Limit > 100 || len(p.After) > 128 || !utf8.ValidString(p.After) {
		return protocol.AgentPage{}, protocol.E("invalid_params", "session page requires a limit up to 100 and a bounded Agent cursor")
	}
	if p.Limit == 0 {
		p.Limit = 40
	}
	page := protocol.AgentPage{Items: []protocol.AgentSummary{}}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		query := "SELECT id,coalesce(parent_id,''),cwd,state,paused FROM agents"
		var args []any
		if p.After != "" {
			var row int64
			if err := tx.QueryRowContext(ctx, "SELECT rowid FROM agents WHERE id=?", p.After).Scan(&row); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return protocol.E("invalid_params", "session page cursor is not a retained Agent")
				}
				return err
			}
			query += " WHERE rowid<?"
			args = append(args, row)
		}
		query += " ORDER BY rowid DESC LIMIT ?"
		args = append(args, p.Limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item protocol.AgentSummary
			if err := rows.Scan(&item.ID, &item.ParentID, &item.Cwd, &item.State, &item.Paused); err != nil {
				return err
			}
			page.Items = append(page.Items, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Items) > p.Limit {
			page.Items = page.Items[:p.Limit]
			page.Next = page.Items[len(page.Items)-1].ID
		}
		return nil
	})
	return page, err
}
