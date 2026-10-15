package runtime

import (
	"context"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func (c *Core) activity(ctx context.Context, id string) (protocol.Activity, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, owners, err := c.Store.Activity(ctx, id)
	if err != nil {
		return result, err
	}
	lister, ok := c.Runner.(interface {
		List(context.Context, process.ListParams) (process.ListPage, error)
	})
	if !ok {
		result.Unavailable = []string{"processes"}
		return result, nil
	}
	after := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		page, err := lister.List(ctx, process.ListParams{After: after, Limit: 100})
		if err != nil {
			result.Unavailable = []string{"processes"}
			return result, nil
		}
		for _, p := range page.Processes {
			if !owners[p.AgentID] || (p.Status != "starting" && p.Status != "running" && p.Status != "unknown" && p.Status != "lost") {
				continue
			}
			if len(result.Items) == protocol.MaxActivityItems {
				result.Truncated = true
				return result, nil
			}
			kind := "pipe"
			if p.PTY {
				kind = "tmux"
			}
			result.Items = append(result.Items, protocol.ActivityItem{Kind: kind, ID: p.ID, AgentID: p.AgentID, State: p.Status})
		}
		if page.Next == "" {
			return result, nil
		}
		if page.Next <= after {
			result.Unavailable = []string{"processes"}
			return result, nil
		}
		after = page.Next
	}
	result.Truncated = true
	return result, nil
}
