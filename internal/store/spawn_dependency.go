package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

// SpawnDependency is prepared by Core against the parent's exact selection.
// Package contains retained manifest/catalog data, not a process or secrets.
type SpawnDependency struct {
	Binding json.RawMessage
	Package resource.Package
}

type SpawnDependencyPublication struct {
	Resource   protocol.ResourceInfo
	Generation int64
}

func validateSpawnDependency(ctx context.Context, tx *sql.Tx, parent string, settings Selection, dependency *SpawnDependency) error {
	if dependency == nil {
		if strings.HasPrefix(settings.Choice.Model, "plugin:") {
			return protocol.E("model_unavailable", "plugin model requires a prepared child dependency")
		}
		return nil
	}
	if !bytes.Equal(dependency.Binding, settings.Binding) {
		return protocol.E("selection_changed", "parent model binding changed during dependency preparation")
	}
	// The provider-specific validation stays in Core. This envelope fences the
	// resource being published against the exact binding copied to the child.
	var snapshot struct {
		Kind      string               `json:"kind"`
		Version   int                  `json:"version"`
		Selection string               `json:"selection"`
		Resource  protocol.ResourceRef `json:"resource"`
	}
	p, err := validatedPackage(dependency.Package)
	if err != nil {
		return err
	}
	if json.Unmarshal(settings.Binding, &snapshot) != nil || snapshot.Kind != "plugin-provider" || snapshot.Version != 1 || snapshot.Selection != settings.Choice.Model || p.Info.Kind != "plugin" || p.Plugin == nil || snapshot.Resource != (protocol.ResourceRef{ID: p.Info.ID, Version: p.Info.Version}) {
		return protocol.E("model_snapshot_invalid", "spawn dependency does not match the selected plugin model")
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT v.package FROM resource_versions v WHERE v.id=? AND v.version=?
 AND EXISTS(SELECT 1 FROM resource_publications r WHERE r.agent_id=? AND r.id=v.id AND r.version=v.version)`, p.Info.ID, p.Info.Version, parent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.E("not_found", "spawn dependency was not published for the parent Agent")
	}
	if err != nil {
		return err
	}
	saved, err := decodePackage(raw)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(saved, p) {
		return protocol.E("invalid_state", "spawn dependency differs from the retained resource version")
	}
	return nil
}

func publishSpawnDependency(ctx context.Context, tx *sql.Tx, child string, p resource.Package) (*SpawnDependencyPublication, error) {
	// validateSpawnDependency already verified the immutable version exists and
	// belongs to the parent. Only ownership/publication is new, within Spawn's
	// transaction; no secondary operation or acceptance receipt is created.
	event, err := appendEvent(ctx, tx, child, "", "resource.published", p.Info)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO resource_publications(agent_id,generation,id,version) VALUES(?,?,?,?)", child, event.Seq, p.Info.ID, p.Info.Version); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO agent_resources(agent_id,id,version,generation) VALUES(?,?,?,?)", child, p.Info.ID, p.Info.Version, event.Seq); err != nil {
		return nil, err
	}
	return &SpawnDependencyPublication{Resource: p.Info, Generation: event.Seq}, nil
}
