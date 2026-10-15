package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

const resourceUpdateSchema = `
CREATE TABLE resource_updates (
 agent_id TEXT NOT NULL REFERENCES agents(id), id TEXT NOT NULL, record TEXT NOT NULL,
 PRIMARY KEY(agent_id,id)
);
CREATE INDEX resource_updates_pending ON resource_updates(agent_id,id)
WHERE json_extract(record,'$.update.state')='preparing';
PRAGMA user_version=13;
`

// ResourcePreparation fences a preparation against a newer attempt. The ready
// pointer is independent: an update never removes a previously usable version.
type ResourcePreparation struct {
	Identity       protocol.ResourceInfo   `json:"identity"`
	Update         protocol.ResourceUpdate `json:"update"`
	BaseGeneration int64                   `json:"base_generation"`
	Owner          string                  `json:"owner"`
	Fingerprint    string                  `json:"fingerprint"`
}

func resourceUpdate(ctx context.Context, tx *sql.Tx, agent, id string) (*ResourcePreparation, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT record FROM resource_updates WHERE agent_id=? AND id=?", agent, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p ResourcePreparation
	if len(raw) > 32<<10 || protocol.StrictDecode([]byte(raw), &p) != nil {
		return nil, protocol.E("invalid_state", "saved resource update is unreadable")
	}
	identity, err := resource.Identity(p.Identity.Kind, p.Identity.Source)
	if err != nil || identity.ID != id || !reflect.DeepEqual(identity, p.Identity) || p.Update.AttemptID == "" || p.Update.OperationID == "" || p.Owner == "" || p.Fingerprint == "" || p.BaseGeneration < 0 {
		return nil, protocol.E("invalid_state", "saved resource update identity changed")
	}
	switch p.Update.State {
	case "preparing", "ready":
		if p.Update.Error != nil {
			return nil, protocol.E("invalid_state", "saved resource update has an unexpected error")
		}
	case "failed":
		if p.Update.Error == nil {
			return nil, protocol.E("invalid_state", "saved resource update has no failure reason")
		}
	default:
		return nil, protocol.E("invalid_state", "saved resource update state is invalid")
	}
	return &p, nil
}

func saveResourceUpdate(ctx context.Context, tx *sql.Tx, agent string, p ResourcePreparation) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, agent, "", "resource.update", struct {
		ID     string                  `json:"resource_id"`
		Kind   string                  `json:"kind"`
		Source string                  `json:"source"`
		Update protocol.ResourceUpdate `json:"update"`
	}{p.Identity.ID, p.Identity.Kind, p.Identity.Source, p.Update}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO resource_updates(agent_id,id,record) VALUES(?,?,?)
ON CONFLICT(agent_id,id) DO UPDATE SET record=excluded.record`, agent, p.Identity.ID, string(raw))
	return err
}

// BeginResourceUpdate rechecks acceptance inside its transaction, so a retry
// cannot leave a false preparing status after the original request committed.
func (s *Store) BeginResourceUpdate(ctx context.Context, req protocol.Request, agent string, identity protocol.ResourceInfo, owner string) (ResourcePreparation, *protocol.Accepted, error) {
	var p ResourcePreparation
	var accepted *protocol.Accepted
	want, err := req.EffectFingerprint()
	if err != nil {
		return p, nil, err
	}
	checked, err := resource.Identity(identity.Kind, identity.Source)
	if err != nil || !reflect.DeepEqual(checked, identity) || owner == "" || len(owner) > 128 || req.OperationID == "" || len(req.OperationID) > 128 {
		return p, nil, protocol.E("invalid_params", "complete resource identity and owner/operation_id of 1–128 bytes are required")
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var hash, raw string
		err := tx.QueryRowContext(ctx, "SELECT fingerprint,result FROM operations WHERE id=?", req.OperationID).Scan(&hash, &raw)
		if err == nil {
			if hash != want {
				return protocol.E("operation_conflict", "operation_id was already used for a different request")
			}
			accepted = &protocol.Accepted{}
			return json.Unmarshal([]byte(raw), accepted)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := worktreeFingerprint(ctx, tx, req.OperationID, want); err != nil {
			return err
		}
		if err := resourceAgent(ctx, tx, agent, true); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (
 SELECT id FROM agent_resources WHERE agent_id=? UNION SELECT id FROM resource_updates WHERE agent_id=?
) WHERE id<>?`, agent, agent, identity.ID).Scan(&count); err != nil {
			return err
		}
		if count >= protocol.MaxAgentResources {
			return protocol.E("resource_limit", "Agent catalog and pending sources exceed 512 resources")
		}
		p = ResourcePreparation{Identity: identity, Owner: owner, Fingerprint: want,
			Update: protocol.ResourceUpdate{AttemptID: protocol.NewID("preparation"), OperationID: req.OperationID, State: "preparing"}}
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM resource_publications WHERE agent_id=?", agent).Scan(&p.BaseGeneration); err != nil {
			return err
		}
		return saveResourceUpdate(ctx, tx, agent, p)
	})
	return p, accepted, err
}

func checkResourcePreparation(ctx context.Context, tx *sql.Tx, req protocol.Request, agent string, p resource.Package, attempt *ResourcePreparation) error {
	if attempt == nil {
		return nil // Legacy internal publisher; runtime preparations always fence.
	}
	want, err := req.EffectFingerprint()
	if err != nil {
		return err
	}
	current, err := resourceUpdate(ctx, tx, agent, p.Info.ID)
	if err != nil {
		return err
	}
	if current == nil || current.Update.State != "preparing" || !reflect.DeepEqual(*current, *attempt) || current.Update.OperationID != req.OperationID || current.Fingerprint != want || current.Identity.ID != p.Info.ID || current.Identity.Source != p.Info.Source || current.Identity.Kind != p.Info.Kind {
		return protocol.E("resource_changed", "resource preparation was superseded; refresh again")
	}
	return nil
}

func finishResourcePreparation(ctx context.Context, tx *sql.Tx, agent string, attempt *ResourcePreparation) error {
	if attempt == nil {
		return nil
	}
	p := *attempt
	p.Update.State = "ready"
	return saveResourceUpdate(ctx, tx, agent, p)
}

// FailResourceUpdate accepts only bounded host-defined diagnostics. It never
// persists arbitrary parser errors, resource bodies or external service output.
func (s *Store) FailResourceUpdate(ctx context.Context, agent string, attempt ResourcePreparation, reason string) error {
	messages := map[string]string{
		"invalid_resource": "Resource validation failed; finish the files and retry refresh.",
		"resource_changed": "Source or publication changed during preparation; retry refresh.",
		"resource_limit":   "Resource exceeds file, byte or catalog limits.",
		"cancelled":        "Resource preparation was cancelled; retry refresh.",
		"timeout":          "Resource connection or preparation timed out; retry refresh.",
		"unavailable":      "Resource connection or credential reference unavailable; retry refresh.",
		"interrupted":      "Core stopped during resource preparation; retry refresh.",
		"internal":         "Resource publication failed; retry refresh after checking core state.",
	}
	message, ok := messages[reason]
	if !ok {
		return protocol.E("invalid_params", "unsupported resource failure kind")
	}
	return s.Transaction(ctx, func(tx *sql.Tx) error {
		current, err := resourceUpdate(ctx, tx, agent, attempt.Identity.ID)
		if err != nil || current == nil {
			return err
		}
		if !reflect.DeepEqual(*current, attempt) || current.Update.State != "preparing" {
			return nil // A late failure cannot replace another attempt's status.
		}
		current.Update.State = "failed"
		current.Update.Error = protocol.E(reason, message)
		current.Update.Error.Data.ResourceID = current.Identity.ID
		current.Update.Error.Data.OperationID = current.Update.OperationID
		return saveResourceUpdate(ctx, tx, agent, *current)
	})
}

// InterruptResourceUpdates runs once for the elected core. It marks only work
// owned by older core instances; current IPC work can already be preparing.
func (s *Store) InterruptResourceUpdates(ctx context.Context, owner string) error {
	if owner == "" {
		return protocol.E("invalid_params", "core owner is required")
	}
	// Paginate by Agent/id and finish each row outside the read transaction.
	// Neither the number of retained Agents nor their histories bounds a scan.
	var afterAgent, afterID string
	for {
		var agent, id string
		var p *ResourcePreparation
		err := s.Transaction(ctx, func(tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, `SELECT agent_id,id FROM resource_updates
WHERE (agent_id,id)>(?,?) AND json_extract(record,'$.owner')<>?
AND json_extract(record,'$.update.state')='preparing' ORDER BY agent_id,id LIMIT 1`, afterAgent, afterID, owner).Scan(&agent, &id)
			if err != nil {
				return err
			}
			p, err = resourceUpdate(ctx, tx, agent, id)
			return err
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.FailResourceUpdate(ctx, agent, *p, "interrupted"); err != nil {
			return err
		}
		afterAgent, afterID = agent, id
	}
}
