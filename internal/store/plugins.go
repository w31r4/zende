package store

import (
	"context"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func (s *Store) PluginCatalog(ctx context.Context, query protocol.PluginCatalogQuery) (resource.PluginCatalog, error) {
	p, err := s.readResourcePackage(ctx, protocol.ResourceRead{AgentID: query.AgentID, ID: query.ID, Version: query.Version})
	if err != nil {
		return resource.PluginCatalog{}, err
	}
	if p.Info.Kind != "plugin" || p.Plugin == nil {
		return resource.PluginCatalog{}, protocol.E("invalid_params", "resource is not a plugin")
	}
	manifest, err := resource.ParsePluginManifest(p.Files[0].Data)
	if err != nil {
		return resource.PluginCatalog{}, err
	}
	return resource.PluginCatalog{ResourceInfo: p.Info, ManifestVersion: manifest.Digest(), SavedCatalog: *p.Plugin}, nil
}
