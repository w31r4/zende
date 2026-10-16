package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func validPluginSource(p PluginCallInput, source *protocol.CallSource) bool {
	if p.Caller == nil {
		return source == nil
	}
	// Provider call IDs are opaque and may exceed host operation ID limits.
	// Their containing intent is already bounded and checked against history.
	return source != nil && source.Caller == *p.Caller && source.RequestID != "" && source.CallID != ""
}

func pluginAgentDispatchable(ctx context.Context, q rowQuerier, agent string) error {
	if err := resourceAgent(ctx, q, agent, true); err != nil {
		return err
	}
	if err := treeWritable(ctx, q, agent); err != nil {
		return err
	}
	var unresolved bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM plugin_calls WHERE agent_id=? AND state='unknown')`, agent).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved {
		return protocol.E("operation_unknown", "an earlier plugin outcome is unverified; inspect the original operation before new calls")
	}
	return nil
}

func pluginCommandScope(ctx context.Context, q rowQuerier, p PluginCallInput) (plugin.Scope, *protocol.CallSource, error) {
	scope := plugin.Scope{AgentID: p.AgentID, TaskID: p.BoundTaskID()}
	source, err := callSource(ctx, q, p.Caller)
	if err != nil {
		return scope, nil, err
	}
	if source == nil {
		err = q.QueryRowContext(ctx, `SELECT cwd FROM agents WHERE id=?`, p.AgentID).Scan(&scope.Cwd)
	} else {
		// A later selection or shell cd must not change an already-issued model
		// request's directory. Older journals kept settings at Task start only.
		err = q.QueryRowContext(ctx, `SELECT coalesce(json_extract(e.payload,'$.settings.cwd'),
 (SELECT json_extract(t.payload,'$.cwd') FROM events t WHERE t.agent_id=e.agent_id AND t.task_id=e.task_id AND t.kind='task.config' ORDER BY t.seq LIMIT 1))
 FROM events e WHERE e.agent_id=? AND e.task_id=? AND e.kind='model.started' AND json_extract(e.payload,'$.request_id')=? ORDER BY e.seq LIMIT 1`, p.AgentID, scope.TaskID, source.RequestID).Scan(&scope.Cwd)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return scope, nil, protocol.E("caller_unknown", "plugin command Agent or original model request was not recorded")
	}
	if err != nil {
		return scope, nil, err
	}
	if !filepath.IsAbs(scope.Cwd) || filepath.Clean(scope.Cwd) != scope.Cwd || len(scope.Cwd) > 4096 || !utf8.ValidString(scope.Cwd) || strings.ContainsRune(scope.Cwd, 0) {
		return scope, nil, protocol.E("invalid_state", "plugin command directory is invalid in the saved request")
	}
	return scope, source, nil
}

// PluginCommandScope resolves provenance without invoking or accepting work.
// BeginPluginCall rechecks it in the dispatch transaction, including Task state.
func (s *Store) PluginCommandScope(ctx context.Context, req protocol.Request) (plugin.Scope, error) {
	p, err := pluginInput(req)
	if err != nil {
		return plugin.Scope{}, err
	}
	if err := pluginAgentDispatchable(ctx, s.db, p.AgentID); err != nil {
		return plugin.Scope{}, err
	}
	scope, _, err := pluginCommandScope(ctx, s.db, p)
	return scope, err
}
