package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

const resourceSchema = `
CREATE TABLE resource_versions (
 id TEXT NOT NULL, version TEXT NOT NULL, info TEXT NOT NULL, package TEXT NOT NULL,
 PRIMARY KEY(id,version)
);
CREATE TABLE agent_resources (
 agent_id TEXT NOT NULL REFERENCES agents(id), id TEXT NOT NULL, version TEXT NOT NULL,
 generation INTEGER NOT NULL,
 PRIMARY KEY(agent_id,id),
 FOREIGN KEY(id,version) REFERENCES resource_versions(id,version)
);
CREATE TABLE resource_publications (
 agent_id TEXT NOT NULL REFERENCES agents(id), generation INTEGER NOT NULL,
 id TEXT NOT NULL, version TEXT NOT NULL,
 PRIMARY KEY(agent_id,generation),
 FOREIGN KEY(id,version) REFERENCES resource_versions(id,version)
);
CREATE TRIGGER resource_versions_no_update BEFORE UPDATE ON resource_versions BEGIN SELECT RAISE(ABORT,'immutable resource version'); END;
CREATE TRIGGER resource_versions_no_delete BEFORE DELETE ON resource_versions BEGIN SELECT RAISE(ABORT,'retained resource version'); END;
CREATE TRIGGER resource_publications_no_update BEFORE UPDATE ON resource_publications BEGIN SELECT RAISE(ABORT,'append-only resource publication'); END;
CREATE TRIGGER resource_publications_no_delete BEFORE DELETE ON resource_publications BEGIN SELECT RAISE(ABORT,'append-only resource publication'); END;
PRAGMA user_version=5;
`

func validatedPackage(p resource.Package) (resource.Package, error) {
	checked, err := resource.BuildPackage(p.Info.Kind, p.Info.Source, p.Entry, p.Files)
	if err != nil || !reflect.DeepEqual(checked, p) {
		return resource.Package{}, protocol.E("invalid_resource", "resource metadata or bytes do not match their version")
	}
	return checked, nil
}
func decodePackage(raw string) (resource.Package, error) {
	if len(raw) > 12<<20 {
		return resource.Package{}, protocol.E("invalid_state", "saved resource exceeds package limit")
	}
	var p resource.Package
	if json.Unmarshal([]byte(raw), &p) != nil {
		return p, protocol.E("invalid_state", "saved resource is unreadable")
	}
	p, err := validatedPackage(p)
	if err != nil {
		return resource.Package{}, protocol.E("invalid_state", "saved resource failed integrity verification")
	}
	return p, nil
}

// baseGeneration is observed before preparation. An intervening publication of
// this resource must not be overwritten by an older, slower preparation.
func (s *Store) PublishResource(ctx context.Context, req protocol.Request, agent string, prepared resource.Package, baseGeneration int64) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		p, err := validatedPackage(prepared)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if err := resourceAgent(ctx, tx, agent, true); err != nil {
			return protocol.Accepted{}, err
		}
		var previous string
		var generation int64
		err = tx.QueryRowContext(ctx, "SELECT version,generation FROM agent_resources WHERE agent_id=? AND id=?", agent, p.Info.ID).Scan(&previous, &generation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, err
		}
		if previous == p.Info.Version {
			return protocol.Accepted{AgentID: agent, Resource: &p.Info}, nil
		}
		if baseGeneration < 0 || generation > baseGeneration {
			return protocol.Accepted{}, protocol.E("resource_changed", "resource was published during preparation; refresh again")
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM agent_resources WHERE agent_id=?", agent).Scan(&count); err != nil {
			return protocol.Accepted{}, err
		}
		if previous == "" && count >= 512 {
			return protocol.Accepted{}, protocol.E("resource_limit", "Agent catalog exceeds 512 resources")
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return protocol.Accepted{}, err
		}
		metadata, err := json.Marshal(p.Info)
		if err != nil {
			return protocol.Accepted{}, err
		}
		// A version is shared only when its immutable snapshot is identical.
		var existing, existingInfo string
		err = tx.QueryRowContext(ctx, "SELECT package,info FROM resource_versions WHERE id=? AND version=?", p.Info.ID, p.Info.Version).Scan(&existing, &existingInfo)
		if err == nil && (existing != string(raw) || existingInfo != string(metadata)) {
			return protocol.Accepted{}, protocol.E("invalid_state", "saved resource version changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, "INSERT INTO resource_versions(id,version,info,package) VALUES(?,?,?,?)", p.Info.ID, p.Info.Version, string(metadata), string(raw)); err != nil {
				return protocol.Accepted{}, err
			}
		}
		event, err := appendEvent(ctx, tx, agent, "", "resource.published", p.Info)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO resource_publications(agent_id,generation,id,version) VALUES(?,?,?,?)", agent, event.Seq, p.Info.ID, p.Info.Version); err != nil {
			return protocol.Accepted{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_resources(agent_id,id,version,generation) VALUES(?,?,?,?)
ON CONFLICT(agent_id,id) DO UPDATE SET version=excluded.version,generation=excluded.generation`, agent, p.Info.ID, p.Info.Version, event.Seq); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{AgentID: agent, Resource: &p.Info}, nil
	})
}

func resourceAgent(ctx context.Context, tx *sql.Tx, agent string, writable bool) error {
	var state string
	err := tx.QueryRowContext(ctx, "SELECT state FROM agents WHERE id=?", agent).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.E("not_found", "Agent not found")
	}
	if err == nil && writable && (state == "copying" || state == "archived") {
		return protocol.E("conflict", "Agent is read-only")
	}
	return err
}

func (s *Store) ResourceStatus(ctx context.Context, target protocol.ResourceTarget) (protocol.ResourceStatus, error) {
	var result protocol.ResourceStatus
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT v.package,r.generation FROM agent_resources r JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id=?`, target.AgentID, target.ID).Scan(&raw, &result.Generation)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.E("not_found", "resource is not registered for this Agent")
		}
		if err != nil {
			return err
		}
		p, err := decodePackage(raw)
		if err != nil {
			return err
		}
		if p.Info.ID != target.ID {
			return protocol.E("invalid_state", "saved resource identity changed")
		}
		result.ResourceInfo, result.State = p.Info, "ready"
		return nil
	})
	return result, err
}

func (s *Store) ReadResource(ctx context.Context, target protocol.ResourceRead) (protocol.ResourceContent, error) {
	if target.Version != "" {
		hash, err := hex.DecodeString(target.Version)
		if err != nil || len(hash) != 32 {
			return protocol.ResourceContent{}, protocol.E("invalid_params", "resource version must be a SHA-256 digest")
		}
	}
	var result protocol.ResourceContent
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		var err error
		if target.Version == "" {
			err = tx.QueryRowContext(ctx, `SELECT v.package FROM agent_resources r JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id=?`, target.AgentID, target.ID).Scan(&raw)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v WHERE v.id=? AND v.version=? AND EXISTS(SELECT 1 FROM resource_publications p WHERE p.agent_id=? AND p.id=v.id AND p.version=v.version)`, target.ID, target.Version, target.AgentID).Scan(&raw)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.E("not_found", "resource version is not published for this Agent")
		}
		if err != nil {
			return err
		}
		p, err := decodePackage(raw)
		if err != nil {
			return err
		}
		if p.Info.ID != target.ID || target.Version != "" && target.Version != p.Info.Version {
			return protocol.E("invalid_state", "saved resource identity changed")
		}
		result, err = p.Content(target.Path)
		return err
	})
	return result, err
}

func (s *Store) Resources(ctx context.Context, query protocol.ResourceList) (protocol.ResourcePage, error) {
	if query.Limit < 0 || query.Limit > 100 || len(query.Query) > 256 || !utf8.ValidString(query.Query) || len(query.After) > 128 || query.Kind != "" && query.Kind != "skill" && query.Kind != "prompt" {
		return protocol.ResourcePage{}, protocol.E("invalid_params", "resource list accepts kind skill/prompt, a 256-byte query and limit 1–100")
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	result := protocol.ResourcePage{Items: []protocol.ResourceInfo{}}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := resourceAgent(ctx, tx, query.AgentID, false); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM resource_publications WHERE agent_id=?", query.AgentID).Scan(&result.Generation); err != nil {
			return err
		}
		// Only metadata is read here, not package bodies or assets. The 512-row
		// catalog bound also bounds filtering work for non-ASCII case folding.
		rows, err := tx.QueryContext(ctx, `SELECT r.id,r.version,v.info FROM agent_resources r JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id>? ORDER BY r.id`, query.AgentID, query.After)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, version, raw string
			if err := rows.Scan(&id, &version, &raw); err != nil {
				return err
			}
			var info protocol.ResourceInfo
			if json.Unmarshal([]byte(raw), &info) != nil || info.ID != id || info.Version != version {
				return protocol.E("invalid_state", "saved resource metadata changed")
			}
			if query.Kind != "" && info.Kind != query.Kind || !strings.Contains(strings.ToLower(info.Name+" "+info.Description), strings.ToLower(query.Query)) {
				continue
			}
			if len(result.Items) == query.Limit {
				result.Next = result.Items[len(result.Items)-1].ID
				break
			}
			result.Items = append(result.Items, info)
		}
		return rows.Err()
	})
	return result, err
}
