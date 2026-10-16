package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

// Publication checks ownership and definition in the same transaction as the
// child pointer update. A parent refresh during byte retrieval must be retried,
// even when the old process remains alive for other leased calls.
func checkPluginResourceParent(ctx context.Context, tx *sql.Tx, agent string, p resource.Package) error {
	if p.Info.PluginOrigin == nil {
		return nil
	}
	origin := p.Info.PluginOrigin
	var version, raw string
	err := tx.QueryRowContext(ctx, `SELECT r.version,v.package FROM agent_resources r
 JOIN resource_versions v ON r.id=v.id AND r.version=v.version WHERE r.agent_id=? AND r.id=?`, agent, origin.Plugin.ID).Scan(&version, &raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && version != origin.Plugin.Version {
		return protocol.E("resource_changed", "plugin publication changed during resource preparation; refresh again")
	}
	if err != nil {
		return err
	}
	parent, err := decodePackage(raw)
	if err != nil {
		return err
	}
	if parent.Info.ID != origin.Plugin.ID || parent.Info.Version != version || parent.Info.Kind != "plugin" || parent.Plugin == nil {
		return protocol.E("invalid_resource", "plugin resource parent is invalid")
	}
	for _, definition := range parent.Plugin.Resources {
		if p.PluginResource != nil && reflect.DeepEqual(definition, *p.PluginResource) {
			return nil
		}
	}
	return protocol.E("invalid_resource", "resource definition does not belong to the published plugin")
}
