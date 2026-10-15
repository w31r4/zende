// Package mcp prepares versioned MCP tool catalogs independently of model prompts.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"zende.local/harness/internal/protocol"
)

const (
	MaxTools           = 4096
	MaxPages           = 128
	MaxDefinitionBytes = 256 << 10
	MaxCatalogBytes    = 16 << 20
	MaxCursorBytes     = 4096
	MaxJSONDepth       = 64
)

var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Definition preserves the SDK's complete tool definition, including schemas,
// annotations and metadata. These remain server data, not host instructions.
type Definition struct {
	Ref      string          `json:"tool_ref"`
	ServerID string          `json:"server_id"`
	Tool     json.RawMessage `json:"definition"`
}

type entry struct {
	definition               Definition
	name, title, description string
}

// Snapshot is immutable after preparation. Returned definitions are copies.
// Its version describes content, not publication order or durable readiness.
type Snapshot struct {
	version string
	entries []entry
	refs    map[string]int
}

// Catalog is the durable, nonsecret representation of a prepared snapshot.
// Restore validates both definitions and their binding-dependent references.
type Catalog struct {
	Version string       `json:"version"`
	Tools   []Definition `json:"tools"`
}

func (s *Snapshot) Export() Catalog {
	d := Catalog{Version: s.version, Tools: make([]Definition, len(s.entries))}
	for i, e := range s.entries {
		d.Tools[i] = copyDefinition(e.definition)
	}
	return d
}

func Restore(ctx context.Context, serverID, bindingVersion string, data Catalog) (*Snapshot, error) {
	if len(data.Tools) > MaxTools {
		return nil, catalogLimit()
	}
	tools := make([]*mcpsdk.Tool, len(data.Tools))
	total := 0
	for i, d := range data.Tools {
		total += len(d.Tool)
		if len(d.Tool) > MaxDefinitionBytes || total > MaxCatalogBytes {
			return nil, catalogLimit()
		}
		if err := json.Unmarshal(d.Tool, &tools[i]); err != nil {
			return nil, invalidCatalog()
		}
	}
	s, err := prepare(ctx, serverID, bindingVersion, func(context.Context, string) (*mcpsdk.ListToolsResult, error) {
		return &mcpsdk.ListToolsResult{Tools: tools}, nil
	})
	if err != nil {
		return nil, err
	}
	if s.version != data.Version {
		return nil, invalidCatalog()
	}
	for i, e := range s.entries {
		if e.definition.Ref != data.Tools[i].Ref || e.definition.ServerID != data.Tools[i].ServerID || string(e.definition.Tool) != string(data.Tools[i].Tool) {
			return nil, invalidCatalog()
		}
	}
	return s, nil
}

func (s *Snapshot) Version() string { return s.version }
func (s *Snapshot) Len() int        { return len(s.entries) }

func (s *Snapshot) Lookup(ref string) (Definition, error) {
	i, ok := s.refs[ref]
	if !ok {
		return Definition{}, protocol.E("stale_reference", "MCP tool reference is unavailable; search again")
	}
	return copyDefinition(s.entries[i].definition), nil
}

func copyDefinition(d Definition) Definition {
	d.Tool = slices.Clone(d.Tool)
	return d
}

type listTools func(context.Context, string) (*mcpsdk.ListToolsResult, error)

// prepare consumes every page before returning anything publishable. It has no
// side effects; callers replace their ready pointer only after success.
func prepare(ctx context.Context, serverID, bindingVersion string, list listTools) (*Snapshot, error) {
	if err := validateIdentity(serverID, bindingVersion); err != nil {
		return nil, err
	}
	s := &Snapshot{refs: map[string]int{}}
	seenNames, seenCursors := map[string]bool{}, map[string]bool{"": true}
	cursor, totalBytes := "", 0
	for page := 0; page < MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := list(ctx, cursor)
		if err != nil {
			return nil, err
		}
		if result == nil || result.Tools == nil {
			return nil, invalidCatalog()
		}
		if len(result.Tools) > MaxTools-len(s.entries) {
			return nil, catalogLimit()
		}
		for _, tool := range result.Tools {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			e, err := prepareEntry(serverID, bindingVersion, tool)
			if err != nil {
				return nil, err
			}
			if seenNames[e.name] {
				return nil, invalidCatalog()
			}
			seenNames[e.name] = true
			totalBytes += len(e.definition.Tool)
			if totalBytes > MaxCatalogBytes {
				return nil, catalogLimit()
			}
			s.entries = append(s.entries, e)
		}
		cursor = result.NextCursor
		if cursor == "" {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			slices.SortFunc(s.entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
			refs := make([]string, len(s.entries))
			for i, e := range s.entries {
				refs[i], s.refs[e.definition.Ref] = e.definition.Ref, i
			}
			s.version = digest([]any{serverID, bindingVersion, refs})
			return s, nil
		}
		if len(cursor) > MaxCursorBytes || !utf8.ValidString(cursor) || seenCursors[cursor] {
			return nil, invalidCatalog()
		}
		seenCursors[cursor] = true
	}
	return nil, catalogLimit()
}

func validateIdentity(serverID, bindingVersion string) error {
	if !serverIDPattern.MatchString(serverID) || bindingVersion == "" || len(bindingVersion) > 256 || !utf8.ValidString(bindingVersion) || strings.ContainsRune(bindingVersion, 0) {
		return protocol.E("invalid_params", "MCP requires a server ID and bounded nonsecret binding version")
	}
	return nil
}

func prepareEntry(serverID, bindingVersion string, tool *mcpsdk.Tool) (entry, error) {
	if tool == nil || tool.Name == "" || len(tool.Name) > 128 || !utf8.ValidString(tool.Name) || strings.IndexFunc(tool.Name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return entry{}, invalidCatalog()
	}
	b, err := json.Marshal(tool)
	if err != nil {
		return entry{}, invalidCatalog()
	}
	if len(b) > MaxDefinitionBytes {
		return entry{}, catalogLimit()
	}
	var value any
	if json.Unmarshal(b, &value) != nil || !boundedJSON(value, 0) {
		return entry{}, invalidCatalog()
	}
	if validateSchema(tool.InputSchema) != nil {
		return entry{}, invalidCatalog()
	}
	if tool.OutputSchema != nil && validateSchema(tool.OutputSchema) != nil {
		return entry{}, invalidCatalog()
	}
	// Re-encode as generic JSON so property order in a server's RawMessage does
	// not change references. This is a supported-definition hash, not wire bytes.
	b, _ = json.Marshal(value)
	d := Definition{Ref: "mcp_tool_" + digest([]any{serverID, bindingVersion, json.RawMessage(b)}), ServerID: serverID, Tool: b}
	return entry{definition: d, name: tool.Name, title: tool.Title, description: tool.Description}, nil
}

func validateSchema(value any) error {
	_, err := compileSchema(value)
	return err
}

func compileSchema(value any) (*jsonschema.Schema, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil || obj == nil || obj["type"] != "object" {
		return nil, invalidCatalog()
	}
	if draft, present := obj["$schema"]; present {
		switch draft {
		case "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema":
		default:
			return nil, invalidCatalog()
		}
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	// Only embedded metaschemas and this resource are available. In particular,
	// disable the library's default file loader as well as remote retrieval.
	compiler.UseLoader(nil)
	const base = "https://zende.invalid/mcp/tool-schema"
	if err := compiler.AddResource(base, obj); err != nil {
		return nil, err
	}
	return compiler.Compile(base)
}

func boundedJSON(v any, depth int) bool {
	if depth > MaxJSONDepth {
		return false
	}
	switch v := v.(type) {
	case map[string]any:
		for _, child := range v {
			if !boundedJSON(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !boundedJSON(child, depth+1) {
				return false
			}
		}
	}
	return true
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func invalidCatalog() error {
	return protocol.E("invalid_resource", "MCP tool catalog contains an invalid, duplicate or incomplete definition")
}
func catalogLimit() error {
	return protocol.E("resource_limit", "MCP tool catalog exceeds preparation limits")
}
