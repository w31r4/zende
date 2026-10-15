package store

import (
	"context"
	"database/sql"

	"zende.local/harness/internal/protocol"
)

// Activity reads the selected Agent tree without loading task inputs/results.
// UNION (not UNION ALL) also bounds traversal of malformed cyclic ancestry.
func (s *Store) Activity(ctx context.Context, id string) (protocol.Activity, map[string]bool, error) {
	result := protocol.Activity{AgentID: id, Items: []protocol.ActivityItem{}}
	owners := map[string]bool{}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		const tree = `WITH RECURSIVE tree(id) AS (
 SELECT id FROM agents WHERE id=? UNION SELECT a.id FROM agents a JOIN tree t ON a.parent_id=t.id
) `
		rows, err := tx.QueryContext(ctx, tree+`SELECT id FROM tree LIMIT 10001`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				rows.Close()
				return err
			}
			owners[owner] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return protocol.E("not_found", "agent not found")
		}
		if len(owners) > 10000 {
			return protocol.E("output_limit", "Agent tree exceeds activity query limit")
		}
		rows, err = tx.QueryContext(ctx, tree+`
 SELECT 'agent',a.id,a.id,a.state FROM agents a JOIN tree t ON t.id=a.id
 WHERE a.id<>? AND (a.state IN ('accepted','running','cancelling','unknown') OR EXISTS
 (SELECT 1 FROM tasks w WHERE w.agent_id=a.id AND w.state IN ('accepted','running','cancelling','unknown')))
 UNION ALL SELECT 'task',w.id,w.agent_id,w.state FROM tasks w JOIN tree t ON t.id=w.agent_id
 WHERE w.state IN ('accepted','running','cancelling','unknown')
 ORDER BY 1,2 LIMIT ?`, id, id, protocol.MaxActivityItems+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item protocol.ActivityItem
			if err := rows.Scan(&item.Kind, &item.ID, &item.AgentID, &item.State); err != nil {
				return err
			}
			if len(result.Items) == protocol.MaxActivityItems {
				result.Truncated = true
				break
			}
			result.Items = append(result.Items, item)
		}
		return rows.Err()
	})
	return result, owners, err
}
