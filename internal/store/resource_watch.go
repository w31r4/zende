package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

// v19 adds discovery provenance to resource preparation records and events.
// Older owners cannot honor the removed-root fence or decode these records.
const resourceDiscoverySchema = `PRAGMA user_version=19;`

// A scan holds at most one page of metadata, including failed first installs.
// Generation and AttemptID fence automatic work against newer manual requests.
type ResourceWatchTarget struct {
	AgentID    string
	Info       protocol.ResourceInfo
	Generation int64
	Update     *protocol.ResourceUpdate
	Discovery  *ResourceDiscovery
}

// Discovery is retained while a first installation is pending or failed. A
// removed root must not be reinstalled by the ordinary registered-source scan.
type ResourceDiscovery struct {
	Root          string                     `json:"root"`
	Configuration protocol.ConfigPublication `json:"configuration"`
}

func (s *Store) ResourceWatchPage(ctx context.Context, afterAgent, afterID string, limit int) ([]ResourceWatchTarget, error) {
	if limit < 1 || limit > 64 {
		return nil, protocol.E("invalid_params", "resource watch page limit must be 1–64")
	}
	rows, err := s.db.QueryContext(ctx, `WITH targets AS (
 SELECT agent_id,id FROM agent_resources UNION SELECT agent_id,id FROM resource_updates
)
SELECT t.agent_id,t.id,coalesce(v.info,''),coalesce(r.version,''),coalesce(r.generation,0),coalesce(u.record,'')
FROM targets t JOIN agents a ON a.id=t.agent_id
LEFT JOIN agent_resources r ON r.agent_id=t.agent_id AND r.id=t.id
LEFT JOIN resource_versions v ON v.id=r.id AND v.version=r.version
LEFT JOIN resource_updates u ON u.agent_id=t.agent_id AND u.id=t.id
WHERE (t.agent_id,t.id)>(?,?) AND a.state NOT IN ('archived','copying')
ORDER BY t.agent_id,t.id LIMIT ?`, afterAgent, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []ResourceWatchTarget
	for rows.Next() {
		var t ResourceWatchTarget
		var id, info, version, update string
		if err := rows.Scan(&t.AgentID, &id, &info, &version, &t.Generation, &update); err != nil {
			return nil, err
		}
		if version != "" {
			if len(info) > 32<<10 || json.Unmarshal([]byte(info), &t.Info) != nil || t.Info.ID != id || t.Info.Version != version || t.Generation <= 0 {
				return nil, protocol.E("invalid_state", "watched resource metadata is unreadable")
			}
			identity, err := resource.Identity(t.Info.Kind, t.Info.Source)
			if err != nil || identity.ID != id || identity.Source != t.Info.Source {
				return nil, protocol.E("invalid_state", "watched resource identity changed")
			}
		}
		if update != "" {
			p, err := decodeResourceUpdate(id, update)
			if err != nil {
				return nil, err
			}
			if version == "" {
				t.Info = p.Identity
				t.Discovery = p.Discovery
			} else if t.Info.Kind != p.Identity.Kind || t.Info.Source != p.Identity.Source {
				return nil, protocol.E("invalid_state", "watched resource update has a different identity")
			}
			t.Update = &p.Update
		}
		if t.Info.ID == "" {
			return nil, protocol.E("invalid_state", "watched resource has no source")
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func checkWatchedResource(ctx context.Context, tx *sql.Tx, target ResourceWatchTarget) error {
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM agent_resources WHERE agent_id=? AND id=?", target.AgentID, target.Info.ID).Scan(&generation); err != nil {
		return err
	}
	current, err := resourceUpdate(ctx, tx, target.AgentID, target.Info.ID)
	if err != nil {
		return err
	}
	wantAttempt, actualAttempt := "", ""
	if target.Update != nil {
		wantAttempt = target.Update.AttemptID
	}
	if current != nil {
		actualAttempt = current.Update.AttemptID
	}
	if generation != target.Generation || wantAttempt != actualAttempt || (current != nil && current.Update.State == "preparing") {
		return protocol.E("resource_changed", "resource changed since observation; observe it again")
	}
	if target.Discovery != nil {
		return checkDiscoveryConfiguration(ctx, tx, *target.Discovery, target.Info)
	}
	return nil
}

func checkDiscoveryConfiguration(ctx context.Context, tx *sql.Tx, discovery ResourceDiscovery, identity protocol.ResourceInfo) error {
	if !discovery.Configuration.Valid() || resource.ValidateResourceDirectories([]string{discovery.Root}) != nil || !resource.DiscoveryContains(discovery.Root, identity) {
		return protocol.E("invalid_params", "invalid discovered source")
	}
	if err := checkConfigPublication(ctx, tx, &discovery.Configuration); err != nil {
		return err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM config_publications").Scan(&generation); err != nil {
		return err
	}
	if generation != discovery.Configuration.Generation {
		return protocol.E("resource_changed", "configuration changed during resource discovery")
	}
	snapshot, err := readConfigSnapshot(ctx, tx, discovery.Configuration.Version)
	if err != nil {
		return err
	}
	for _, root := range snapshot.Effective.Config.ResourceDirs {
		if root == discovery.Root {
			return nil
		}
	}
	return protocol.E("resource_changed", "resource directory is no longer configured")
}

// UndiscoveredResources advances one Agent at a time, even when all entries
// already exist. It does not load retained histories or whole Agent catalogs.
func (s *Store) UndiscoveredResources(ctx context.Context, afterAgent string, sources []protocol.ResourceInfo) (string, []ResourceWatchTarget, error) {
	if len(sources) > resource.MaxDiscoveryPage {
		return "", nil, protocol.E("invalid_params", "discovery source page exceeds 16 entries")
	}
	var agent string
	var targets []ResourceWatchTarget
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT coalesce(min(id),'') FROM agents WHERE id>? AND state NOT IN ('archived','copying')", afterAgent).Scan(&agent)
		if err != nil || agent == "" {
			return err
		}
		for _, identity := range sources {
			checked, err := resource.Identity(identity.Kind, identity.Source)
			if err != nil || checked.ID != identity.ID || checked.Source != identity.Source {
				return protocol.E("invalid_params", "discovery source identity is invalid")
			}
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT id FROM agent_resources WHERE agent_id=? AND id=?
 UNION ALL SELECT id FROM resource_updates WHERE agent_id=? AND id=?)`, agent, identity.ID, agent, identity.ID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				targets = append(targets, ResourceWatchTarget{AgentID: agent, Info: checked})
			}
		}
		return nil
	})
	return agent, targets, err
}
