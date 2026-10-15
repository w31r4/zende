package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

// ParsePluginManifest accepts YAML/JSON with identical scalar semantics.
// Conversion through JSON prevents YAML's string coercions from changing types.
func ParsePluginManifest(data []byte) (plugin.Manifest, error) {
	fail := func() (plugin.Manifest, error) {
		return plugin.Manifest{}, protocol.E("invalid_resource", "invalid plugin manifest; use schema_version 1 with environment references")
	}
	if len(data) == 0 || len(data) > plugin.MaxManifestBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fail()
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&doc) != nil || decoder.Decode(new(any)) != io.EOF || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fail()
	}
	var valid func(*yaml.Node, int) bool
	valid = func(n *yaml.Node, depth int) bool {
		if depth > 8 || n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!null" {
			return false
		}
		switch n.Kind {
		case yaml.ScalarNode:
			if n.Tag != "!!str" && n.Tag != "!!int" {
				return false
			}
		case yaml.MappingNode:
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return false
				}
				seen[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if !valid(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !valid(&doc, 0) {
		return fail()
	}
	var value any
	if doc.Decode(&value) != nil {
		return fail()
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fail()
	}
	var manifest plugin.Manifest
	if protocol.StrictDecode(raw, &manifest) != nil || manifest.Validate() != nil {
		return fail()
	}
	return manifest, nil
}

type PreparedPlugin struct {
	Source   string          `json:"source"`
	Version  string          `json:"version"`
	Manifest plugin.Manifest `json:"manifest"`
	File     []byte          `json:"file"`
}

func readPluginManifest(ctx context.Context, source string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, protocol.E("invalid_resource", "plugin manifest unavailable")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, protocol.E("invalid_resource", "plugin manifest must be a regular file")
	}
	if before.Size() > plugin.MaxManifestBytes {
		return nil, protocol.E("resource_limit", "plugin manifest exceeds 128 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, plugin.MaxManifestBytes+1))
	if err != nil {
		return nil, protocol.E("invalid_resource", "plugin manifest unreadable")
	}
	after, err := file.Stat()
	if err != nil || len(data) != int(before.Size()) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, protocol.E("resource_changed", "plugin manifest changed during preparation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// PreparePlugin only validates/initializes a candidate. Runtime publication and
// retirement are separate operations; an error never yields a prepared package.
// The optional failed instance is closed and available for diagnostics only.
func PreparePlugin(ctx context.Context, source string, env func(string) string) (PreparedPlugin, *plugin.Instance, error) {
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return PreparedPlugin{}, nil, protocol.E("invalid_resource", "plugin source must be an absolute clean path")
	}
	data, err := readPluginManifest(ctx, source)
	if err != nil {
		return PreparedPlugin{}, nil, err
	}
	manifest, err := ParsePluginManifest(data)
	if err != nil {
		return PreparedPlugin{}, nil, err
	}
	if env == nil {
		env = os.Getenv
	}
	resolved := make(map[string]string, len(manifest.Env))
	for name, ref := range manifest.Env {
		value, err := ResolveCredential(ref, env)
		if err != nil {
			return PreparedPlugin{}, nil, protocol.E("unavailable", "plugin credential reference unavailable")
		}
		resolved[name] = value
	}
	instance, err := plugin.Start(ctx, source, manifest, resolved)
	if err != nil {
		return PreparedPlugin{}, instance, err
	}
	again, err := readPluginManifest(ctx, source)
	if err == nil && !bytes.Equal(data, again) {
		err = protocol.E("resource_changed", "plugin manifest changed during initialization")
	}
	if err == nil && instance.Info().State != "ready" {
		err = protocol.E("unavailable", "plugin exited during preparation")
	}
	if err != nil {
		instance.Close()
		return PreparedPlugin{}, instance, err
	}
	return PreparedPlugin{Source: source, Version: manifest.Digest(), Manifest: manifest, File: bytes.Clone(data)}, instance, nil
}
