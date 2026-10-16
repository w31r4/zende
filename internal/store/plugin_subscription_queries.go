package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"zende.local/harness/internal/protocol"
)

type PluginSubscriptionsPage struct {
	Items []PluginSubscriptionReceipt `json:"items"`
	Next  string                      `json:"next"`
	More  bool                        `json:"more"`
}

func (s *Store) PluginSubscriptions(ctx context.Context, p protocol.PluginSubscriptionQuery) (PluginSubscriptionsPage, error) {
	out := PluginSubscriptionsPage{Items: []PluginSubscriptionReceipt{}, Next: p.After}
	if !pluginID(p.AgentID, true) || !pluginID(p.ResourceID, false) || !pluginID(p.After, false) || p.Limit < 0 || p.Limit > 100 {
		return out, protocol.E("invalid_params", "subscription list requires Agent and limit 1–100")
	}
	if p.Limit == 0 {
		p.Limit = 20
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := resourceAgent(ctx, tx, p.AgentID, false); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT subscription_id FROM plugin_subscriptions WHERE agent_id=? AND subscription_id>? AND (?='' OR json_extract(intent,'$.request.params.resource_id')=?) ORDER BY subscription_id LIMIT ?`, p.AgentID, p.After, p.ResourceID, p.ResourceID, p.Limit+1)
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
		out.More = len(ids) > p.Limit
		total := 512
		for _, id := range ids[:min(len(ids), p.Limit)] {
			r, err := subscriptionRecord(ctx, tx, protocol.PluginSubscriptionTarget{AgentID: p.AgentID, SubscriptionID: id})
			if err != nil {
				return err
			}
			if r == nil {
				return protocol.E("invalid_state", "subscription state history is missing")
			}
			raw, _ := json.Marshal(r.Receipt)
			if total+len(raw)+1 > protocol.MaxFrameBytes-4096 {
				if len(out.Items) == 0 {
					return protocol.E("output_limit", "subscription metadata exceeds the IPC response budget")
				}
				out.More = true
				break
			}
			total += len(raw) + 1
			out.Items, out.Next = append(out.Items, r.Receipt), id
		}
		return nil
	})
	if err != nil {
		return PluginSubscriptionsPage{}, err
	}
	return out, nil
}
