package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"zende.local/harness/internal/protocol"
)

const MaxDefinitions = 256
const MaxCatalogBytes = 4 << 20
const MaxDefinitionBytes = 256 << 10
const MaxArgumentBytes = 1 << 20
const MaxResultBytes = 4 << 20

type Callable struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

type Definition struct {
	Callable
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type Catalog struct {
	version string
	entries []callableEntry
}

type callableEntry struct {
	definition    Definition
	input, output *jsonschema.Schema
}

func (c *Catalog) Version() string { return c.version }
func (c *Catalog) Definitions() []Definition {
	out := make([]Definition, len(c.entries))
	for n, e := range c.entries {
		out[n] = cloneDefinition(e.definition)
	}
	return out
}

func cloneDefinition(d Definition) Definition {
	d.InputSchema = bytes.Clone(d.InputSchema)
	d.OutputSchema = bytes.Clone(d.OutputSchema)
	return d
}

// Discover binds one immutable callable catalog to this process. Updates are
// prepared on a new instance; running calls never reinterpret their schemas.
func (i *Instance) Discover(ctx context.Context) (*Catalog, error) {
	i.mu.Lock()
	ready, catalog := i.info.State == "ready", i.catalog
	i.mu.Unlock()
	if !ready {
		return nil, protocol.E("unavailable", "plugin connection is not ready")
	}
	if catalog != nil {
		return catalog, nil
	}
	ctx, cancel := context.WithTimeout(ctx, InitializeTimeout)
	defer cancel()
	info := i.Info()
	kinds := []string{}
	for _, kind := range []string{"tools", "commands"} {
		if slices.Contains(info.Capabilities, kind) {
			kinds = append(kinds, kind)
		}
	}
	raw, err := i.exchange(ctx, "plugin.describe", struct {
		Kinds []string `json:"kinds"`
	}{kinds})
	if err != nil {
		return nil, err
	}
	catalog, err = compileCatalog(info, raw)
	if err != nil {
		i.stop(asFailure(err))
		return nil, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.info.State != "ready" {
		return nil, protocol.E("unavailable", "plugin exited during discovery")
	}
	if i.catalog != nil {
		if i.catalog.version != catalog.version {
			return nil, protocol.E("resource_changed", "plugin definitions changed within one instance")
		}
		return i.catalog, nil
	}
	i.catalog = catalog
	return catalog, nil
}

var callableName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

func invalidCatalog() error {
	return protocol.E("plugin_protocol", "invalid plugin callable catalog; inspect definitions and schemas")
}

func compileCatalog(info Info, raw []byte) (*Catalog, error) {
	if len(raw) > MaxCatalogBytes || !utf8.Valid(raw) {
		return nil, invalidCatalog()
	}
	var catalog struct {
		Tools    []Callable `json:"tools"`
		Commands []Callable `json:"commands"`
	}
	if protocol.StrictDecode(raw, &catalog) != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'{'}) || len(catalog.Tools)+len(catalog.Commands) > MaxDefinitions {
		return nil, invalidCatalog()
	}
	if len(catalog.Tools) > 0 && !slices.Contains(info.Capabilities, "tools") || len(catalog.Commands) > 0 && !slices.Contains(info.Capabilities, "commands") {
		return nil, invalidCatalog()
	}
	out := &Catalog{}
	for _, group := range []struct {
		kind        string
		definitions []Callable
	}{{"tool", catalog.Tools}, {"command", catalog.Commands}} {
		seen := map[string]bool{}
		for _, d := range group.definitions {
			encoded, _ := json.Marshal(d)
			if !callableName.MatchString(d.Name) || seen[d.Name] || d.Description == "" || len(d.Description) > 8192 || len(encoded) > MaxDefinitionBytes {
				return nil, invalidCatalog()
			}
			seen[d.Name] = true
			input, err := compileSchema(d.InputSchema)
			if err != nil {
				return nil, invalidCatalog()
			}
			var output *jsonschema.Schema
			if len(d.OutputSchema) > 0 {
				output, err = compileSchema(d.OutputSchema)
				if err != nil {
					return nil, invalidCatalog()
				}
			}
			// Canonical object ordering, retaining exact numeric values.
			value, err := decodeValue(encoded, MaxDefinitionBytes)
			if err != nil {
				return nil, invalidCatalog()
			}
			canonical, _ := json.Marshal(value)
			if json.Unmarshal(canonical, &d) != nil {
				return nil, invalidCatalog()
			}
			ref := digest([]any{info.ID, info.ManifestVersion, group.kind, json.RawMessage(canonical)})
			out.entries = append(out.entries, callableEntry{Definition{Callable: d, Kind: group.kind, Ref: "plugin_call_" + ref}, input, output})
		}
	}
	slices.SortFunc(out.entries, func(a, b callableEntry) int {
		return strings.Compare(a.definition.Kind+":"+a.definition.Name, b.definition.Kind+":"+b.definition.Name)
	})
	out.version = digest(out.Definitions())
	return out, nil
}

func digest(v any) string { raw, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func decodeValue(raw []byte, maxBytes int) (any, error) {
	if len(raw) == 0 || len(raw) > maxBytes || !utf8.Valid(raw) {
		return nil, protocol.E("invalid_arguments", "invalid or oversized plugin JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || !boundedValue(v, 0) || protocol.StrictDecode(raw, new(json.RawMessage)) != nil {
		return nil, protocol.E("invalid_arguments", "plugin JSON requires unique keys, depth at most 64 and bounded numbers")
	}
	return v, nil
}

func boundedValue(v any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch v := v.(type) {
	case map[string]any:
		for _, child := range v {
			if !boundedValue(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !boundedValue(child, depth+1) {
				return false
			}
		}
	case json.Number:
		s := v.String()
		if len(s) > 1024 {
			return false
		}
		if at := strings.IndexAny(s, "eE"); at >= 0 {
			n, err := strconv.Atoi(s[at+1:])
			return err == nil && n >= -1024 && n <= 1024
		}
	}
	return true
}

func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	v, err := decodeValue(raw, MaxDefinitionBytes)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok || obj["type"] != "object" {
		return nil, invalidCatalog()
	}
	if draft, present := obj["$schema"]; present {
		if draft != "http://json-schema.org/draft-07/schema#" && draft != "https://json-schema.org/draft-07/schema#" && draft != "https://json-schema.org/draft/2020-12/schema" {
			return nil, invalidCatalog()
		}
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(nil)
	const base = "https://zende.invalid/plugin/callable-schema"
	if err := compiler.AddResource(base, obj); err != nil {
		return nil, err
	}
	return compiler.Compile(base)
}
