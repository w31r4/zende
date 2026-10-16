package store

import (
	"context"

	"zende.local/harness/internal/protocol"
)

type PluginTaskStatus struct {
	Pending int
	Unknown int
}

func pluginTaskStatus(ctx context.Context, q rowQuerier, agent, task string) (PluginTaskStatus, error) {
	var out PluginTaskStatus
	err := q.QueryRowContext(ctx, `SELECT coalesce(sum(state IN ('dispatched','cancelling')),0),coalesce(sum(state='unknown'),0) FROM plugin_calls WHERE agent_id=? AND task_id=?`, agent, task).Scan(&out.Pending, &out.Unknown)
	return out, err
}

func (s *Store) PluginTaskStatus(ctx context.Context, agent, task string) (PluginTaskStatus, error) {
	if !pluginID(agent, true) || !pluginID(task, true) {
		return PluginTaskStatus{}, protocol.E("invalid_params", "Agent and Task are required for plugin status")
	}
	return pluginTaskStatus(ctx, s.db, agent, task)
}
