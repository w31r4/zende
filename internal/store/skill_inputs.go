package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const skillInputSchema = `
ALTER TABLE tasks ADD COLUMN skills TEXT NOT NULL DEFAULT '[]';
ALTER TABLE questions ADD COLUMN skills TEXT NOT NULL DEFAULT '[]';
CREATE TRIGGER task_skills_no_update BEFORE UPDATE OF skills ON tasks BEGIN SELECT RAISE(ABORT,'immutable task skills'); END;
CREATE TRIGGER question_skills_no_update BEFORE UPDATE OF skills ON questions WHEN OLD.state!='pending' BEGIN SELECT RAISE(ABORT,'immutable answer skills'); END;
PRAGMA user_version=6;
`

// Resolve only the exact, already published versions. Both acceptance and
// context delivery use this path; filesystem changes are irrelevant.
func selectedSkills(ctx context.Context, tx *sql.Tx, agent string, refs []protocol.ResourceRef) ([]model.Block, error) {
	if len(refs) > protocol.MaxInputSkills {
		return nil, protocol.E("resource_limit", "input supports up to 8 selected skills")
	}
	var blocks []model.Block
	seen := map[string]bool{}
	for _, ref := range refs {
		digest, err := hex.DecodeString(ref.Version)
		if err != nil || len(digest) != 32 || ref.ID == "" || len(ref.ID) > 128 || seen[ref.ID] {
			return nil, protocol.E("invalid_params", "skills require distinct resource IDs and exact SHA-256 versions")
		}
		seen[ref.ID] = true
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v WHERE v.id=? AND v.version=? AND EXISTS(SELECT 1 FROM resource_publications p WHERE p.agent_id=? AND p.id=v.id AND p.version=v.version)`, ref.ID, ref.Version, agent).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, protocol.E("not_found", "selected skill version is not published for this Agent")
		}
		if err != nil {
			return nil, err
		}
		p, err := decodePackage(raw)
		if err != nil {
			return nil, err
		}
		if p.Info.ID != ref.ID || p.Info.Version != ref.Version || p.Info.Kind != "skill" {
			return nil, protocol.E("invalid_resource", "selected resource must be the exact skill version")
		}
		// A user selection also permits disable-model-invocation skills. The
		// metadata identifies instructions, not a new system/developer authority.
		metadata, err := json.Marshal(struct {
			protocol.ResourceRef
			Name   string `json:"name"`
			Source string `json:"source"`
		}{ref, p.Info.Name, p.Info.Source})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, model.Block{Type: "text", Text: "User-selected skill " + string(metadata) + "\nRead auxiliary files with resource read at this version; scripts use process tools.\n\n" + p.Instructions})
	}
	return blocks, nil
}
