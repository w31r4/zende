package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func BuildPluginResourcePackage(source string, origin protocol.PluginResourceOrigin, definition plugin.ResourceDefinition, files []File) (Package, error) {
	parent, kind, name, err := protocol.ParsePluginResourceSource(source)
	if err != nil || parent != origin.Plugin.ID || !origin.Valid() || definition.Kind != kind || definition.Name != name || origin.DefinitionRef != definition.Ref {
		return Package{}, protocol.E("invalid_resource", "invalid plugin resource source or origin")
	}
	input := make([]plugin.ResourceFile, len(files))
	for n, f := range files {
		input[n] = plugin.ResourceFile{Path: f.Path, Data: f.Data, Executable: f.Executable}
	}
	if err := plugin.ValidateResourceFiles(definition, input); err != nil {
		return Package{}, err
	}
	p, err := buildInstructionPackage(kind, source, definition.Entry, files)
	if err != nil {
		return Package{}, err
	}
	if p.Info.Name != definition.Name || p.Info.Description != definition.Description || p.Info.DisableModelInvocation != definition.DisableModelInvocation {
		return Package{}, protocol.E("invalid_resource", "plugin instruction metadata differs from its catalog")
	}
	definition.Files = slices.Clone(definition.Files)
	p.Info.PluginOrigin, p.PluginResource = &origin, &definition
	raw, _ := json.Marshal(struct {
		Kind, Entry string
		Origin      protocol.PluginResourceOrigin
		Definition  plugin.ResourceDefinition
		Files       []File
	}{kind, p.Entry, origin, definition, p.Files})
	sum := sha256.Sum256(raw)
	p.Info.Version = hex.EncodeToString(sum[:])
	return p, nil
}
