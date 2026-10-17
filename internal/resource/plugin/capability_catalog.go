package plugin

import (
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const MaxProviderModels = 256

// Provider describes model choices, not a callable tool schema. Credentials
// remain in the plugin's launch environment, outside discovery data.
type Provider struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Models      []protocol.ModelInfo `json:"models"`
}

type ProviderDefinition struct {
	Provider
	Ref string `json:"ref"`
}

// Resource describes an immutable file bundle. Each file is verified against
// this inventory when read; discovery does not inject instructions or run files.
type Resource struct {
	Name                   string                      `json:"name"`
	Description            string                      `json:"description"`
	Kind                   string                      `json:"kind"`
	Entry                  string                      `json:"entry"`
	DisableModelInvocation bool                        `json:"disable_model_invocation,omitempty"`
	Files                  []protocol.ResourceFileInfo `json:"files"`
}

type ResourceDefinition struct {
	Resource
	Ref string `json:"ref"`
}

type catalogReply struct {
	Tools     []Callable        `json:"tools,omitempty"`
	Commands  []Callable        `json:"commands,omitempty"`
	Providers []Provider        `json:"providers,omitempty"`
	Resources []Resource        `json:"resources,omitempty"`
	Events    []EventCapability `json:"events,omitempty"`
}

func (c *Catalog) Providers() []ProviderDefinition {
	var out []ProviderDefinition
	for _, d := range c.providers {
		d.Models = slices.Clone(d.Models)
		for n := range d.Models {
			d.Models[n].Efforts = slices.Clone(d.Models[n].Efforts)
		}
		out = append(out, d)
	}
	return out
}

func (c *Catalog) Resources() []ResourceDefinition {
	var out []ResourceDefinition
	for _, d := range c.resources {
		d.Files = slices.Clone(d.Files)
		out = append(out, d)
	}
	return out
}

func (c *Catalog) digest() string {
	// Preserve existing tools/commands-only versions and persisted packages.
	if len(c.providers)+len(c.resources)+len(c.events) == 0 {
		return digest(c.Definitions())
	}
	return digest(struct {
		Definitions []Definition         `json:"definitions"`
		Providers   []ProviderDefinition `json:"providers,omitempty"`
		Resources   []ResourceDefinition `json:"resources,omitempty"`
		Events      []EventDefinition    `json:"events,omitempty"`
	}{c.Definitions(), c.providers, c.resources, c.Events()})
}

func boundedID(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return r <= 32 || r == 127 }) < 0
}

func (c *Catalog) compileCapabilities(info Info, reply catalogReply) error {
	if len(reply.Providers) > 0 && !slices.Contains(info.Capabilities, "providers") || len(reply.Resources) > 0 && !slices.Contains(info.Capabilities, "resources") {
		return invalidCatalog()
	}
	seen, models := map[string]bool{}, 0
	for _, p := range reply.Providers {
		if !callableName.MatchString(p.Name) || seen[p.Name] || strings.TrimSpace(p.Description) == "" || len(p.Description) > 8192 || len(p.Models) == 0 {
			return invalidCatalog()
		}
		seen[p.Name] = true
		models += len(p.Models)
		if models > MaxProviderModels {
			return invalidCatalog()
		}
		ids := map[string]bool{}
		for n := range p.Models {
			m := &p.Models[n]
			// Plugin providers use the portable host function contract. Native
			// Responses discovery belongs to the built-in endpoint adapter.
			if m.ToolSearch != "" && m.ToolSearch != "host" {
				return invalidCatalog()
			}
			if !boundedID(m.ID, 256) || ids[m.ID] || len(m.Label) > 1024 || m.ContextWindow < 1 || m.MaxOutput < 1 || m.MaxOutput >= m.ContextWindow || len(m.Efforts) > 16 {
				return invalidCatalog()
			}
			ids[m.ID] = true
			if m.Efforts == nil {
				m.Efforts = []string{}
			}
			efforts := map[string]bool{}
			for _, e := range m.Efforts {
				if !boundedID(e, 64) || efforts[e] {
					return invalidCatalog()
				}
				efforts[e] = true
			}
		}
		slices.SortFunc(p.Models, func(a, b protocol.ModelInfo) int { return strings.Compare(a.ID, b.ID) })
		encoded, _ := json.Marshal(p)
		if len(encoded) > MaxDefinitionBytes {
			return invalidCatalog()
		}
		ref := "plugin_provider_" + digest([]any{info.ID, info.ManifestVersion, "provider", p})
		c.providers = append(c.providers, ProviderDefinition{Provider: p, Ref: ref})
	}
	seen = map[string]bool{}
	for _, r := range reply.Resources {
		if err := validResource(r); err != nil || seen[r.Kind+":"+r.Name] {
			return invalidCatalog()
		}
		seen[r.Kind+":"+r.Name] = true
		slices.SortFunc(r.Files, func(a, b protocol.ResourceFileInfo) int { return strings.Compare(a.Path, b.Path) })
		encoded, _ := json.Marshal(r)
		if len(encoded) > MaxDefinitionBytes {
			return invalidCatalog()
		}
		ref := "plugin_resource_" + digest([]any{info.ID, info.ManifestVersion, "resource", r})
		c.resources = append(c.resources, ResourceDefinition{Resource: r, Ref: ref})
	}
	slices.SortFunc(c.providers, func(a, b ProviderDefinition) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(c.resources, func(a, b ResourceDefinition) int { return strings.Compare(a.Kind+":"+a.Name, b.Kind+":"+b.Name) })
	return nil
}

func validResource(r Resource) error {
	if !namePattern.MatchString(r.Name) || len(r.Name) > 64 || strings.TrimSpace(r.Description) == "" || utf8.RuneCountInString(r.Description) > 1024 || (r.Kind != "skill" && r.Kind != "prompt") || r.Entry == "" || r.Entry != path.Base(r.Entry) || len(r.Files) == 0 || len(r.Files) > protocol.MaxResourceFiles || r.Kind == "skill" && r.Entry != "SKILL.md" || r.Kind == "prompt" && len(r.Files) != 1 {
		return invalidCatalog()
	}
	seen, total, found := map[string]bool{}, int64(0), false
	for _, f := range r.Files {
		hash, err := hex.DecodeString(f.SHA256)
		if f.Path == "" || f.Path == "." || f.Path == ".." || path.Clean(f.Path) != f.Path || path.IsAbs(f.Path) || strings.HasPrefix(f.Path, "../") || strings.ContainsAny(f.Path, "\x00\\") || len(f.Path) > 1024 || seen[f.Path] || f.Bytes < 0 || f.Bytes > protocol.MaxResourceFileBytes || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != f.SHA256 {
			return invalidCatalog()
		}
		seen[f.Path] = true
		total += f.Bytes
		if f.Path == r.Entry && f.Bytes > 0 {
			found = true
		}
	}
	if !found || total > protocol.MaxResourceBytes {
		return invalidCatalog()
	}
	return nil
}
