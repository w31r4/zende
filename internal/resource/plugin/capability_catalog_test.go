package plugin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
)

func capabilityFixture() (Manifest, catalogReply) {
	m := Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", Command: "/does-not-run", ProtocolVersions: []int{1}, Capabilities: []string{"tools", "commands", "providers", "resources"}}
	c := catalogReply{
		Tools:     []Callable{{Name: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Providers: []Provider{{Name: "models", Description: "Model choices", Models: []protocol.ModelInfo{{ID: "model/a", ContextWindow: 32000, MaxOutput: 4096, Efforts: []string{"low", "high"}, Images: true, NativeCategories: true}}}},
		Resources: []Resource{{Name: "example", Description: "Instruction package", Kind: "skill", Entry: "SKILL.md", Files: []protocol.ResourceFileInfo{{Path: "SKILL.md", Bytes: 100, SHA256: strings.Repeat("0", 64)}, {Path: "scripts/run.sh", Bytes: 50, SHA256: strings.Repeat("a", 64), Executable: true}}}},
	}
	return m, c
}

func compileFixture(m Manifest, reply catalogReply) (*Catalog, error) {
	raw, _ := json.Marshal(reply)
	return compileCatalog(Info{Identity: Identity{ID: m.ID, Version: m.Version}, ManifestVersion: m.Digest(), Capabilities: m.Capabilities}, raw)
}

func TestPluginCapabilityCatalogBindingsAndRestoration(t *testing.T) {
	m, reply := capabilityFixture()
	c, err := compileFixture(m, reply)
	if err != nil {
		t.Fatal(err)
	}
	saved := c.Export()
	if len(saved.Providers) != 1 || len(saved.Resources) != 1 || len(saved.Definitions) != 1 {
		t.Fatal(saved)
	}
	restored, err := RestoreCatalog(m, saved)
	if err != nil || !reflect.DeepEqual(restored.Export(), saved) {
		t.Fatal(restored, err)
	}

	// Adding another kind changes only the catalog version, not prior refs.
	_, onlyTools := capabilityFixture()
	onlyTools.Providers, onlyTools.Resources = nil, nil
	base, err := compileFixture(m, onlyTools)
	if err != nil || base.Version() != digest(base.Definitions()) || base.Definitions()[0].Ref != saved.Definitions[0].Ref || base.Version() == c.Version() {
		t.Fatal(base, err)
	}
	if _, err := RestoreCatalog(m, base.Export()); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(struct {
		CatalogVersion string       `json:"catalog_version"`
		Definitions    []Definition `json:"definitions"`
	}{base.Version(), base.Definitions()})
	current, _ := json.Marshal(base.Export())
	if string(legacy) != string(current) {
		t.Fatal("legacy serialized catalog changed")
	}
	extra := reply.Providers[0]
	extra.Name = "another"
	reply.Providers = append(reply.Providers, extra)
	more, err := compileFixture(m, reply)
	if err != nil || more.Providers()[1].Ref != saved.Providers[0].Ref || more.Resources()[0].Ref != saved.Resources[0].Ref {
		t.Fatal(more, err)
	}

	// Definitions returned to callers do not alias the immutable catalog.
	copy := c.Export()
	copy.Providers[0].Models[0].Efforts[0] = "changed"
	copy.Resources[0].Files[0].Path = "changed"
	if !reflect.DeepEqual(c.Export(), saved) {
		t.Fatal("catalog mutated through exported data")
	}
	for name, alter := range map[string]func(*SavedCatalog){
		"provider-ref": func(s *SavedCatalog) { s.Providers[0].Ref = "forged" },
		"resource-ref": func(s *SavedCatalog) { s.Resources[0].Ref = "forged" },
		"model":        func(s *SavedCatalog) { s.Providers[0].Models[0].MaxOutput++ },
		"digest":       func(s *SavedCatalog) { s.Resources[0].Files[0].SHA256 = strings.Repeat("b", 64) },
		"executable":   func(s *SavedCatalog) { s.Resources[0].Files[1].Executable = false },
		"source-kind":  func(s *SavedCatalog) { s.Resources[0].Kind = "prompt" },
		"removed":      func(s *SavedCatalog) { s.Providers = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := c.Export()
			alter(&copy)
			_, err := RestoreCatalog(m, copy)
			requireFailure(t, err, "plugin_protocol")
		})
	}
	m.Version = "2"
	if _, err := RestoreCatalog(m, saved); err == nil {
		t.Fatal("new manifest retained old refs")
	}
}

func TestPluginCapabilityCatalogRejectsInvalidDefinitions(t *testing.T) {
	for name, alter := range map[string]func(*Manifest, *catalogReply){
		"undeclared-provider": func(m *Manifest, _ *catalogReply) { m.Capabilities = []string{"tools", "resources"} },
		"undeclared-resource": func(m *Manifest, _ *catalogReply) { m.Capabilities = []string{"tools", "providers"} },
		"empty-provider":      func(_ *Manifest, c *catalogReply) { c.Providers[0].Models = nil },
		"provider-name":       func(_ *Manifest, c *catalogReply) { c.Providers[0].Name = "with space" },
		"duplicate-provider":  func(_ *Manifest, c *catalogReply) { c.Providers = append(c.Providers, c.Providers[0]) },
		"model-context":       func(_ *Manifest, c *catalogReply) { c.Providers[0].Models[0].ContextWindow = 0 },
		"model-output":        func(_ *Manifest, c *catalogReply) { c.Providers[0].Models[0].MaxOutput = 32000 },
		"model-id":            func(_ *Manifest, c *catalogReply) { c.Providers[0].Models[0].ID = "a\nb" },
		"duplicate-model": func(_ *Manifest, c *catalogReply) {
			c.Providers[0].Models = append(c.Providers[0].Models, c.Providers[0].Models[0])
		},
		"duplicate-effort":     func(_ *Manifest, c *catalogReply) { c.Providers[0].Models[0].Efforts = []string{"low", "low"} },
		"resource-kind":        func(_ *Manifest, c *catalogReply) { c.Resources[0].Kind = "tool" },
		"resource-name":        func(_ *Manifest, c *catalogReply) { c.Resources[0].Name = "Upper Case" },
		"resource-description": func(_ *Manifest, c *catalogReply) { c.Resources[0].Description = strings.Repeat("界", 1025) },
		"resource-no-entry":    func(_ *Manifest, c *catalogReply) { c.Resources[0].Files = c.Resources[0].Files[1:] },
		"resource-size":        func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[0].Bytes = -1 },
		"resource-traversal":   func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[1].Path = "../escape" },
		"resource-alias":       func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[1].Path = "a/../b" },
		"resource-absolute":    func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[1].Path = "/outside" },
		"resource-digest":      func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[0].SHA256 = strings.Repeat("A", 64) },
		"resource-duplicate-file": func(_ *Manifest, c *catalogReply) {
			c.Resources[0].Files = append(c.Resources[0].Files, c.Resources[0].Files[0])
		},
		"duplicate-resource": func(_ *Manifest, c *catalogReply) { c.Resources = append(c.Resources, c.Resources[0]) },
		"definition-limit":   func(_ *Manifest, c *catalogReply) { c.Providers = make([]Provider, MaxDefinitions) },
		"model-limit": func(_ *Manifest, c *catalogReply) {
			m := c.Providers[0].Models[0]
			c.Providers[0].Models = nil
			for n := 0; n <= MaxProviderModels; n++ {
				m.ID = fmt.Sprint(n)
				c.Providers[0].Models = append(c.Providers[0].Models, m)
			}
		},
		"effort-limit": func(_ *Manifest, c *catalogReply) {
			c.Providers[0].Models[0].Efforts = nil
			for n := 0; n < 17; n++ {
				c.Providers[0].Models[0].Efforts = append(c.Providers[0].Models[0].Efforts, fmt.Sprint(n))
			}
		},
		"file-limit": func(_ *Manifest, c *catalogReply) { c.Resources[0].Files[0].Bytes = protocol.MaxResourceFileBytes + 1 },
		"bundle-limit": func(_ *Manifest, c *catalogReply) {
			file := c.Resources[0].Files[0]
			file.Bytes = protocol.MaxResourceFileBytes
			for n := 0; n < 9; n++ {
				file.Path = fmt.Sprintf("extra-%d", n)
				c.Resources[0].Files = append(c.Resources[0].Files, file)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, reply := capabilityFixture()
			alter(&m, &reply)
			_, err := compileFixture(m, reply)
			requireFailure(t, err, "plugin_protocol")
		})
	}
}
