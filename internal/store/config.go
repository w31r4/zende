package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

const configSchema = `
CREATE TABLE config_versions (
 version TEXT PRIMARY KEY, snapshot TEXT NOT NULL
);
CREATE TABLE config_publications (
 generation INTEGER PRIMARY KEY CHECK(generation>0),
 version TEXT NOT NULL REFERENCES config_versions(version),
 operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id)
   DEFERRABLE INITIALLY DEFERRED
);
CREATE TRIGGER config_versions_no_update BEFORE UPDATE ON config_versions
 BEGIN SELECT RAISE(ABORT,'immutable configuration version'); END;
CREATE TRIGGER config_versions_no_delete BEFORE DELETE ON config_versions
 BEGIN SELECT RAISE(ABORT,'configuration versions retained'); END;
CREATE TRIGGER config_publications_no_update BEFORE UPDATE ON config_publications
 BEGIN SELECT RAISE(ABORT,'append-only configuration publication'); END;
CREATE TRIGGER config_publications_no_delete BEFORE DELETE ON config_publications
 BEGIN SELECT RAISE(ABORT,'configuration publications retained'); END;
PRAGMA user_version=18;
`

type PublishedConfig struct {
	Publication protocol.ConfigPublication `json:"publication"`
	Snapshot    resource.ConfigSnapshot    `json:"snapshot"`
}

func configVersionValid(version string) bool {
	digest, err := hex.DecodeString(version)
	return err == nil && len(digest) == 32 && version == hex.EncodeToString(digest)
}

func readConfigSnapshot(ctx context.Context, q rowQuerier, version string) (resource.ConfigSnapshot, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT snapshot FROM config_versions WHERE version=?", version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return resource.ConfigSnapshot{}, protocol.E("not_found", "configuration version was not published")
	}
	if err != nil {
		return resource.ConfigSnapshot{}, err
	}
	var snapshot resource.ConfigSnapshot
	if len(raw) > resource.MaxConfigBytes || protocol.StrictDecode([]byte(raw), &snapshot) != nil || snapshot.Version != version || snapshot.Validate() != nil {
		return resource.ConfigSnapshot{}, protocol.E("invalid_state", "saved configuration version is unreadable or changed")
	}
	return snapshot, nil
}

// CurrentConfig is nil only before the first publication. Historical reads do
// not consult today's files, environment, defaults or credentials.
func (s *Store) CurrentConfig(ctx context.Context) (*PublishedConfig, error) {
	var out *PublishedConfig
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var p protocol.ConfigPublication
		err := tx.QueryRowContext(ctx, "SELECT generation,version,operation_id FROM config_publications ORDER BY generation DESC LIMIT 1").Scan(&p.Generation, &p.Version, &p.OperationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if p.Generation < 1 || !configVersionValid(p.Version) || p.OperationID == "" {
			return protocol.E("invalid_state", "configuration publication identity is invalid")
		}
		snapshot, err := readConfigSnapshot(ctx, tx, p.Version)
		var known *protocol.Error
		if errors.As(err, &known) && known.Data.Kind == "not_found" {
			return protocol.E("invalid_state", "published configuration version is missing")
		}
		if err != nil {
			return err
		}
		out = &PublishedConfig{Publication: p, Snapshot: snapshot}
		return nil
	})
	return out, err
}

func (s *Store) ConfigVersion(ctx context.Context, version string) (resource.ConfigSnapshot, error) {
	if !configVersionValid(version) {
		return resource.ConfigSnapshot{}, protocol.E("invalid_params", "configuration version must be a lowercase SHA-256 digest")
	}
	return readConfigSnapshot(ctx, s.db, version)
}

// PublishConfig commits bytes, the current-version journal and acceptance in
// one transaction. The owner prepares providers first, rechecks sources before
// this call and replaces its in-memory bundle only after commit succeeds.
// expectedGeneration fences competing preparations, including A -> B -> A.
func (s *Store) PublishConfig(ctx context.Context, req protocol.Request, snapshot resource.ConfigSnapshot, expectedGeneration int64) (protocol.Accepted, error) {
	if old, found, err := s.AcceptedRequest(ctx, req); found || err != nil {
		return old, err
	}
	if req.Method != "config.refresh" || expectedGeneration < 0 || req.OperationID == "" || len(req.OperationID) > 128 {
		return protocol.Accepted{}, protocol.E("invalid_params", "configuration publication requires config.refresh, an operation ID and a nonnegative generation")
	}
	if err := snapshot.Validate(); err != nil {
		return protocol.Accepted{}, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return protocol.Accepted{}, err
	}
	return s.accept(ctx, req, func(tx *sql.Tx) (protocol.Accepted, error) {
		var generation int64
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM config_publications").Scan(&generation); err != nil {
			return protocol.Accepted{}, err
		}
		if generation != expectedGeneration {
			return protocol.Accepted{}, protocol.E("config_changed", "configuration was published during preparation; prepare again")
		}
		if generation == math.MaxInt64 {
			return protocol.Accepted{}, protocol.E("resource_limit", "configuration publication generation is exhausted")
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO config_versions(version,snapshot) VALUES(?,?)", snapshot.Version, string(raw)); err != nil {
			return protocol.Accepted{}, err
		}
		saved, err := readConfigSnapshot(ctx, tx, snapshot.Version)
		if err != nil {
			return protocol.Accepted{}, err
		}
		existing, err := json.Marshal(saved)
		if err != nil || string(existing) != string(raw) {
			return protocol.Accepted{}, protocol.E("invalid_state", "configuration version already identifies different bytes")
		}
		publication := protocol.ConfigPublication{Generation: generation + 1, Version: snapshot.Version, OperationID: req.OperationID}
		if _, err := tx.ExecContext(ctx, "INSERT INTO config_publications(generation,version,operation_id) VALUES(?,?,?)", publication.Generation, publication.Version, publication.OperationID); err != nil {
			return protocol.Accepted{}, err
		}
		return protocol.Accepted{Config: &publication}, nil
	})
}

// This journal orders configuration publications only. Agent events retain
// their own per-session sequence numbers.
func (s *Store) ConfigHistory(ctx context.Context, after int64, limit int) (protocol.ConfigHistory, error) {
	out := protocol.ConfigHistory{Items: []protocol.ConfigPublication{}, Next: after}
	if after < 0 || limit < 0 || limit > 100 {
		return out, protocol.E("invalid_params", "configuration history requires a nonnegative cursor and limit at most 100")
	}
	if limit == 0 {
		limit = 20
	}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(generation),0) FROM config_publications").Scan(&out.Latest); err != nil {
			return err
		}
		if after > out.Latest {
			return protocol.E("cursor_gap", "configuration cursor is ahead of retained publications")
		}
		rows, err := tx.QueryContext(ctx, "SELECT generation,version,operation_id FROM config_publications WHERE generation>? ORDER BY generation LIMIT ?", after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p protocol.ConfigPublication
			if err := rows.Scan(&p.Generation, &p.Version, &p.OperationID); err != nil {
				return err
			}
			if p.Generation != out.Next+1 || !configVersionValid(p.Version) || p.OperationID == "" {
				return protocol.E("invalid_state", "configuration publication journal has a gap or invalid identity")
			}
			out.Items = append(out.Items, p)
			out.Next = p.Generation
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if out.Next < out.Latest && len(out.Items) < limit {
			return protocol.E("invalid_state", "configuration publication journal is incomplete")
		}
		return nil
	})
	return out, err
}
