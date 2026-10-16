package resource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

// BuildPluginPackage verifies persisted bytes without starting a process or
// resolving credentials. File bytes and the verified directory form a version.
func BuildPluginPackage(source string, files []File, saved plugin.SavedCatalog) (Package, error) {
	identity, err := Identity("plugin", source)
	if err != nil || identity.Source != source || len(files) != 1 || files[0].Path != filepath.Base(source) || !ValidResourcePath(files[0].Path) || files[0].Executable {
		return Package{}, protocol.E("invalid_resource", "invalid plugin package identity")
	}
	manifest, err := ParsePluginManifest(files[0].Data)
	if err != nil {
		return Package{}, err
	}
	catalog, err := plugin.RestoreCatalog(manifest, saved)
	if err != nil {
		return Package{}, err
	}
	verified := catalog.Export()
	p := Package{Info: identity, Entry: files[0].Path, Files: []File{{Path: files[0].Path, Data: bytes.Clone(files[0].Data)}}, Plugin: &verified}
	p.Info.Name = manifest.ID
	p.Info.Description = "Plugin " + manifest.ID + " " + manifest.Version
	p.Info.Files = 1
	raw, _ := json.Marshal(verified)
	p.Info.Bytes = int64(len(files[0].Data) + len(raw))
	hash, _ := json.Marshal(struct {
		Files   []File
		Catalog plugin.SavedCatalog
	}{p.Files, verified})
	sum := sha256.Sum256(hash)
	p.Info.Version = hex.EncodeToString(sum[:])
	return p, nil
}

// PreparePluginPackage initializes and discovers a complete candidate before
// publication. The caller owns the returned process only on success.
func PreparePluginPackage(ctx context.Context, source string, env func(string) string) (Package, *plugin.Instance, error) {
	// Initialization and discovery share one preparation budget, leaving the
	// CLI's outer timeout room to persist status and reclaim a failed candidate.
	ctx, cancel := context.WithTimeout(ctx, plugin.InitializeTimeout)
	defer cancel()
	prepared, instance, err := PreparePlugin(ctx, source, env)
	if err != nil {
		return Package{}, nil, err
	}
	owned := false
	defer func() {
		if !owned {
			instance.Close()
		}
	}()
	catalog, err := instance.Discover(ctx)
	if err != nil {
		return Package{}, nil, err
	}
	again, err := readPluginManifest(ctx, source)
	if err != nil {
		return Package{}, nil, err
	}
	if !bytes.Equal(prepared.File, again) {
		return Package{}, nil, protocol.E("resource_changed", "plugin manifest changed during discovery")
	}
	p, err := BuildPluginPackage(source, []File{{Path: filepath.Base(source), Data: prepared.File}}, catalog.Export())
	if err != nil {
		return Package{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return Package{}, nil, err
	}
	if instance.Info().State != "ready" {
		return Package{}, nil, protocol.E("unavailable", "plugin exited during preparation")
	}
	owned = true
	return p, instance, nil
}

// PluginCatalog is retained discovery data, not a live-readiness assertion.
type PluginCatalog struct {
	protocol.ResourceInfo
	ManifestVersion string `json:"manifest_version"`
	plugin.SavedCatalog
}
