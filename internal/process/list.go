package process

import (
	"context"
	"encoding/json"
	"sort"

	"zende.local/harness/internal/protocol"
)

type ListParams struct {
	AgentID string `json:"agent_id,omitempty"`
	After   string `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type ListPage struct {
	Processes []State `json:"processes"`
	Next      string  `json:"next,omitempty"`
}

// List observes runner-owned records, including completed/unknown processes.
// Pagination is by immutable identity, not by mutable status or array offset.
func (m *Manager) List(ctx context.Context, params ListParams) (ListPage, error) {
	page := ListPage{Processes: []State{}}
	if params.Limit < 0 || params.Limit > 100 || (params.After != "" && !processID.MatchString(params.After)) {
		return page, protocol.E("invalid_params", "process list limit must be 1–100 and cursor a process ID")
	}
	if params.Limit == 0 {
		params.Limit = 20
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		if id > params.After {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	sort.Strings(ids)
	bytes := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		e, err := m.get(id)
		if err != nil {
			return page, err
		}
		e.mu.Lock()
		matches := params.AgentID == "" || e.state.AgentID == params.AgentID
		e.mu.Unlock()
		if !matches {
			continue
		}
		if len(page.Processes) == params.Limit {
			page.Next = page.Processes[len(page.Processes)-1].ID
			break
		}
		state, err := m.Inspect(ctx, id)
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return page, err
		}
		if len(encoded) > protocol.MaxFrameBytes-4096 {
			return page, protocol.E("output_limit", "process record exceeds the IPC budget; inspect original stored state")
		}
		if bytes+len(encoded) > protocol.MaxFrameBytes-4096 {
			page.Next = page.Processes[len(page.Processes)-1].ID
			break
		}
		bytes += len(encoded) + 1
		page.Processes = append(page.Processes, state)
	}
	return page, nil
}
