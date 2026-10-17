package store

import (
	"context"
	"database/sql"

	"zende.local/harness/internal/protocol"
)

const recoveryDependencyLimit = 32

// Metadata only, read at the journal cursor. Counts cover the full set; each
// sample is bounded and keeps the identity used by the existing detail readers.
type RecoveryDependencies struct {
	Mailbox   RecoveryMailbox   `json:"mailbox"`
	Resources RecoveryResources `json:"resources"`
	Plugins   RecoveryPlugins   `json:"plugins"`
}

type RecoveryMailbox struct {
	Pending int64               `json:"pending"`
	Results int64               `json:"pending_results"`
	Items   []protocol.MailInfo `json:"items"`
	More    bool                `json:"more"`
}

type RecoveryResourceUpdate struct {
	ID      string                  `json:"resource_id"`
	Kind    string                  `json:"kind"`
	Version string                  `json:"retained_version,omitempty"`
	Update  protocol.ResourceUpdate `json:"update"`
}

type RecoveryResources struct {
	Pending int64                    `json:"preparing"`
	Failed  int64                    `json:"failed"`
	Items   []RecoveryResourceUpdate `json:"items"`
	More    bool                     `json:"more"`
}

type RecoveryPluginCall struct {
	OperationID   string `json:"operation_id"`
	ResourceID    string `json:"resource_id"`
	Version       string `json:"resource_version"`
	InstanceID    string `json:"instance_id"`
	DefinitionRef string `json:"definition_ref"`
	RequestID     string `json:"request_id,omitempty"`
	State         string `json:"state"`
}

type RecoveryPlugins struct {
	Pending int64                `json:"pending"`
	Unknown int64                `json:"unknown"`
	Items   []RecoveryPluginCall `json:"items"`
	More    bool                 `json:"more"`
}

func (s *Store) RecoveryDependencies(ctx context.Context, agent, task string, cursor int64) (RecoveryDependencies, error) {
	var out RecoveryDependencies
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT a.seq FROM agents a JOIN tasks t ON t.agent_id=a.id WHERE a.id=? AND t.id=?`, agent, task).Scan(&current); err != nil {
			return err
		}
		if current != cursor {
			return protocol.E("recovery_conflict", "recovery dependencies changed after the journal read")
		}
		var err error
		out.Mailbox, err = recoveryMailbox(ctx, tx, agent)
		if err != nil {
			return err
		}
		out.Resources, err = recoveryResources(ctx, tx, agent)
		if err != nil {
			return err
		}
		out.Plugins, err = recoveryPlugins(ctx, tx, agent, task)
		return err
	})
	return out, err
}

func recoveryMailbox(ctx context.Context, tx *sql.Tx, agent string) (RecoveryMailbox, error) {
	out := RecoveryMailbox{Items: []protocol.MailInfo{}}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(result_task IS NOT NULL),0) FROM mailbox
 WHERE agent_id=? AND delivered_seq=0`, agent).Scan(&out.Pending, &out.Results); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+mailColumns+` FROM mailbox m LEFT JOIN tasks t ON t.id=m.task_id
 WHERE m.agent_id=? AND m.delivered_seq=0 ORDER BY m.seq LIMIT ?`, agent, recoveryDependencyLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		mail, err := scanMail(rows, false)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, mail.MailInfo)
	}
	out.More = out.Pending > int64(len(out.Items))
	return out, rows.Err()
}

func recoveryResources(ctx context.Context, tx *sql.Tx, agent string) (RecoveryResources, error) {
	out := RecoveryResources{Items: []RecoveryResourceUpdate{}}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(json_extract(record,'$.update.state')='preparing'),0),
 coalesce(sum(json_extract(record,'$.update.state')='failed'),0) FROM resource_updates WHERE agent_id=?`, agent).Scan(&out.Pending, &out.Failed); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.id,u.record,coalesce(r.version,'') FROM resource_updates u
 LEFT JOIN agent_resources r ON r.agent_id=u.agent_id AND r.id=u.id WHERE u.agent_id=?
 AND json_extract(u.record,'$.update.state') IN ('preparing','failed')
 ORDER BY (json_extract(u.record,'$.update.state')='preparing') DESC,u.id LIMIT ?`, agent, recoveryDependencyLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item RecoveryResourceUpdate
		var raw string
		if err := rows.Scan(&item.ID, &raw, &item.Version); err != nil {
			return out, err
		}
		prepared, err := decodeResourceUpdate(item.ID, raw)
		if err != nil {
			return out, err
		}
		item.Kind, item.Update = prepared.Identity.Kind, prepared.Update
		out.Items = append(out.Items, item)
	}
	out.More = out.Pending+out.Failed > int64(len(out.Items))
	return out, rows.Err()
}

func recoveryPlugins(ctx context.Context, tx *sql.Tx, agent, task string) (RecoveryPlugins, error) {
	out := RecoveryPlugins{Items: []RecoveryPluginCall{}}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(state IN ('dispatched','cancelling')),0),coalesce(sum(state='unknown'),0)
 FROM plugin_calls WHERE agent_id=? AND task_id=?`, agent, task).Scan(&out.Pending, &out.Unknown); err != nil {
		return out, err
	}
	// Select identity fields, not the retained invocation (which includes args).
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,state,request_id,
 json_extract(intent,'$.request.params.resource_id'),json_extract(intent,'$.resource_version'),
 json_extract(intent,'$.binding.instance_id'),json_extract(intent,'$.binding.definition_ref')
 FROM plugin_calls WHERE agent_id=? AND task_id=? AND state IN ('dispatched','cancelling','unknown')
 ORDER BY (state='unknown') DESC,operation_id LIMIT ?`, agent, task, recoveryDependencyLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item RecoveryPluginCall
		if err := rows.Scan(&item.OperationID, &item.State, &item.RequestID, &item.ResourceID, &item.Version, &item.InstanceID, &item.DefinitionRef); err != nil {
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	out.More = out.Pending+out.Unknown > int64(len(out.Items))
	return out, rows.Err()
}
