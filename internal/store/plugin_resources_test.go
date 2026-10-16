package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func retainedPluginChild(t *testing.T, f *pluginJournalFixture) resource.Package {
	t.Helper()
	d := f.pkg.Plugin.Resources[1]
	_, got, err := f.instance.ReadResource(t.Context(), d.Ref)
	if err != nil {
		t.Fatal(err)
	}
	var files []resource.File
	for _, file := range got {
		files = append(files, resource.File{Path: file.Path, Data: file.Data, Executable: file.Executable})
	}
	origin := protocol.PluginResourceOrigin{Plugin: protocol.ResourceRef{ID: f.pkg.Info.ID, Version: f.pkg.Info.Version}, DefinitionRef: d.Ref}
	p, err := resource.BuildPluginResourcePackage("plugin://"+f.pkg.Info.ID+"/skill/"+d.Name, origin, d, files)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginResourcePublicationChecksParentAndRetainsHistory(t *testing.T) {
	f := newPluginJournal(t, "")
	f.manifest.Capabilities = append(f.manifest.Capabilities, "resources")
	f.publish(t, "2")
	p := retainedPluginChild(t, f)
	_, a := publication(t, f.s, f.agent, "child", p, f.generation)
	if !reflect.DeepEqual(a.Resource.PluginOrigin, p.Info.PluginOrigin) {
		t.Fatal(a)
	}
	// Publishing a syntactically valid bundle is insufficient: its exact
	// definition must belong to this Agent's current parent publication.
	definition := *p.PluginResource
	definition.Ref = "plugin_resource_" + strings.Repeat("f", 64)
	origin := *p.Info.PluginOrigin
	origin.DefinitionRef = definition.Ref
	forged, err := resource.BuildPluginResourcePackage(p.Info.Source, origin, definition, p.Files)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := protocol.NewRequest("resource.register", "forged", protocol.RegisterResource{AgentID: f.agent, Kind: "skill", Path: p.Info.Source})
	if _, err := f.s.PublishResource(t.Context(), r, f.agent, forged, a.ResourceGeneration); err == nil {
		t.Fatal("forged definition published")
	}
	other := protocol.CreateAgent{Cwd: t.TempDir()}
	or, _ := protocol.NewRequest("agent.create", "other", other)
	b, err := f.s.Create(t.Context(), or, other, 8)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = protocol.NewRequest("resource.register", "foreign", protocol.RegisterResource{AgentID: b.AgentID, Kind: "skill", Path: p.Info.Source})
	if _, err := f.s.PublishResource(t.Context(), r, b.AgentID, p, 0); err == nil {
		t.Fatal("foreign parent accepted")
	}
	f.publish(t, "3")
	r, _ = protocol.NewRequest("resource.refresh", "stale-child", protocol.ResourceTarget{AgentID: f.agent, ID: p.Info.ID})
	_, err = f.s.PublishResource(t.Context(), r, f.agent, p, f.generation)
	wantPluginError(t, err, "resource_changed")
	// A stale parent's check also precedes the unchanged-child fast path.
	current := retainedPluginChild(t, f)
	publication(t, f.s, f.agent, "current-child", current, f.generation)
	f.instance.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []resource.Package{p, current} {
		content, err := s.ReadResource(t.Context(), protocol.ResourceRead{AgentID: f.agent, ID: p.Info.ID, Version: p.Info.Version})
		if err != nil || !reflect.DeepEqual(content.PluginOrigin, p.Info.PluginOrigin) || !strings.Contains(content.Text, p.Instructions) {
			t.Fatal(content, err)
		}
	}
	// The retained file bytes and provenance are both integrity-checked.
	for _, alter := range []func(*resource.Package){
		func(p *resource.Package) { p.Files[0].Data[0] = '!' },
		func(p *resource.Package) { p.Info.PluginOrigin.Plugin.Version = strings.Repeat("a", 64) },
		func(p *resource.Package) { p.PluginResource.Ref = "changed" },
		func(p *resource.Package) {
			p.PluginResource.Files[0].Executable = !p.PluginResource.Files[0].Executable
		},
	} {
		raw, _ := json.Marshal(current)
		var copy resource.Package
		if err := json.Unmarshal(raw, &copy); err != nil {
			t.Fatal(err)
		}
		alter(&copy)
		if _, err := validatedPackage(copy); err == nil {
			t.Fatal("modified package accepted")
		}
	}
}
