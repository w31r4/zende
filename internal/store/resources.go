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
	if p.Info.Kind == "mcp" && p.MCP != nil {
		checked, err = resource.BuildMCPPackage(p.Info.Source, p.Files, *p.MCP)
	}
	if p.Info.Kind == "plugin" && p.Plugin != nil {
		checked, err = resource.BuildPluginPackage(p.Info.Source, p.Files, *p.Plugin)
	}
	if p.PluginResource != nil && p.Info.PluginOrigin != nil {
		checked, err = resource.BuildPluginResourcePackage(p.Info.Source, *p.Info.PluginOrigin, *p.PluginResource, p.Files)
	}
	if err != nil || !reflect.DeepEqual(checked, p) {
		return resource.Package{}, protocol.E("invalid_resource", "resource metadata or bytes do not match their version")
	}
	return checked, nil
}
func decodePackage(raw string) (resource.Package, error) {
	if len(raw) > 20<<20 {
		return resource.Package{}, protocol.E("invalid_state", "saved resource exceeds package limit")
	}
	var p resource.Package
	if json.Unmarshal([]byte(raw), &p) != nil {
		return p, protocol.E("invalid_state", "saved resource is unreadable")
	}
	if p.Info.Kind != "mcp" && len(raw) > 12<<20 {
		return resource.Package{}, protocol.E("invalid_state", "saved resource exceeds package limit")
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
	return s.publishResource(ctx, req, agent, prepared, baseGeneration, nil)
}

func (s *Store) PublishResourceUpdate(ctx context.Context, req protocol.Request, agent string, prepared resource.Package, attempt ResourcePreparation) (protocol.Accepted, error) {
	return s.publishResource(ctx, req, agent, prepared, attempt.BaseGeneration, &attempt)
}

func (s *Store) publishResource(ctx context.Context, req protocol.Request, agent string, prepared resource.Package, baseGeneration int64, attempt *ResourcePreparation) (protocol.Accepted, error) {
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		p, err := validatedPackage(prepared)
		if err != nil {
			return protocol.Accepted{}, err
		}
		if err := resourceAgent(ctx, tx, agent, true); err != nil {
			return protocol.Accepted{}, err
		}
		if err := checkResourcePreparation(ctx, tx, req, agent, p, attempt); err != nil {
			return protocol.Accepted{}, err
		}
		if err := checkPluginResourceParent(ctx, tx, agent, p); err != nil {
			return protocol.Accepted{}, err
		}
		var previous string
		var generation int64
		err = tx.QueryRowContext(ctx, "SELECT version,generation FROM agent_resources WHERE agent_id=? AND id=?", agent, p.Info.ID).Scan(&previous, &generation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return protocol.Accepted{}, err
		}
		if previous == p.Info.Version {
			return protocol.Accepted{AgentID: agent, Resource: &p.Info, ResourceGeneration: generation}, finishResourcePreparation(ctx, tx, agent, attempt)
		}
		if baseGeneration < 0 || generation > baseGeneration {
			return protocol.Accepted{}, protocol.E("resource_changed", "resource was published during preparation; refresh again")
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM agent_resources WHERE agent_id=?", agent).Scan(&count); err != nil {
			return protocol.Accepted{}, err
		}
		if previous == "" && count >= protocol.MaxAgentResources {
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
		return protocol.Accepted{AgentID: agent, Resource: &p.Info, ResourceGeneration: event.Seq}, finishResourcePreparation(ctx, tx, agent, attempt)
	})
}

// ResourceSnapshot is read in the same transaction as the model context. Only
// metadata is loaded; bodies and assets stay behind versioned resource.read.
type ResourceSnapshot struct {
	Catalog protocol.ResourceCatalog
	Items   []protocol.ResourceStatus
}

func resourceSnapshot(ctx context.Context, tx *sql.Tx, agent string) (ResourceSnapshot, error) {
	r := ResourceSnapshot{Catalog: protocol.ResourceCatalog{Entries: []protocol.ResourceRef{}}}
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM resource_publications WHERE agent_id=?", agent).Scan(&r.Catalog.Generation); err != nil {
		return r, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.version,r.generation,v.info FROM agent_resources r
 JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? ORDER BY r.id LIMIT ?`, agent, protocol.MaxAgentResources+1)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, version, raw string
		var item protocol.ResourceStatus
		if err := rows.Scan(&id, &version, &item.Generation, &raw); err != nil {
			return r, err
		}
		if len(raw) > 32<<10 || protocol.StrictDecode([]byte(raw), &item.ResourceInfo) != nil || item.ID != id || item.Version != version || item.Generation <= 0 || item.Generation > r.Catalog.Generation {
			return r, protocol.E("invalid_state", "saved resource metadata changed")
		}
		item.State = "ready"
		r.Items = append(r.Items, item)
		r.Catalog.Entries = append(r.Catalog.Entries, protocol.ResourceRef{ID: id, Version: version})
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	if !r.Catalog.Valid() {
		return r, protocol.E("invalid_state", "saved resource catalog is inconsistent")
	}
	return r, nil
}

func resourceAgent(ctx context.Context, tx rowQuerier, agent string, writable bool) error {
	if writable {
		if err := copyWritable(ctx, tx, agent); err != nil {
			return err
		}
	}
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

// Offline discovery can reconnect or start a local MCP host. Copy admission
// therefore applies before creating such a runtime, even on a read API.
func (s *Store) ResourceRuntimeAllowed(ctx context.Context, agent string) error {
	return resourceAgent(ctx, s.db, agent, true)
}

func (s *Store) ResourceStatus(ctx context.Context, target protocol.ResourceTarget) (protocol.ResourceStatus, error) {
	var result protocol.ResourceStatus
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := resourceAgent(ctx, tx, target.AgentID, false); err != nil {
			return err
		}
		update, err := resourceUpdate(ctx, tx, target.AgentID, target.ID)
		if err != nil {
			return err
		}
		if update != nil {
			result.ResourceInfo, result.State, result.Update = update.Identity, update.Update.State, &update.Update
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT v.package,r.generation FROM agent_resources r JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id=?`, target.AgentID, target.ID).Scan(&raw, &result.Generation)
		if errors.Is(err, sql.ErrNoRows) {
			if update != nil && update.Update.State != "ready" {
				return nil
			}
			if update != nil {
				return protocol.E("invalid_state", "ready resource update has no publication")
			}
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
	p, err := s.readResourcePackage(ctx, target)
	if err != nil {
		return protocol.ResourceContent{}, err
	}
	return p.Content(target.Path)
}

func (s *Store) readResourcePackage(ctx context.Context, target protocol.ResourceRead) (resource.Package, error) {
	if target.Version != "" {
		hash, err := hex.DecodeString(target.Version)
		if err != nil || len(hash) != 32 {
			return resource.Package{}, protocol.E("invalid_params", "resource version must be a SHA-256 digest")
		}
	}
	var result resource.Package
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
		result = p
		return nil
	})
	return result, err
}

func (s *Store) Resources(ctx context.Context, query protocol.ResourceList) (protocol.ResourcePage, error) {
	if query.Limit < 0 || query.Limit > 100 || len(query.Query) > 256 || !utf8.ValidString(query.Query) || len(query.After) > 128 || query.Kind != "" && query.Kind != "skill" && query.Kind != "prompt" && query.Kind != "mcp" && query.Kind != "plugin" {
		return protocol.ResourcePage{}, protocol.E("invalid_params", "resource list accepts kind skill/prompt/mcp/plugin, a 256-byte query and limit 1–100")
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

// PublishedResource loads the current immutable package for a managed runtime
// on a writable Agent. It does not infer live readiness from the publication.
func (s *Store) PublishedResource(ctx context.Context, target protocol.ResourceTarget) (resource.Package, int64, error) {
	var p resource.Package
	var generation int64
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := resourceAgent(ctx, tx, target.AgentID, true); err != nil {
			return err
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT v.package,r.generation FROM agent_resources r JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id=?`, target.AgentID, target.ID).Scan(&raw, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.E("not_found", "resource is not published for this Agent")
		}
		if err != nil {
			return err
		}
		p, err = decodePackage(raw)
		if err == nil && p.Info.ID != target.ID {
			return protocol.E("invalid_state", "saved resource identity changed")
		}
		return err
	})
	return p, generation, err
}
