package resource

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func readPluginResourceFixture(t *testing.T, mode string) (Package, *plugin.Instance) {
	t.Helper()
	m, source := packagePluginFixture(t)
	m.Capabilities = append(m.Capabilities, "resources")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	writePluginFixture(t, source, m)
	p, instance, err := PreparePluginPackage(t.Context(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	return p, instance
}

func buildChildFixture(t *testing.T, p Package, instance *plugin.Instance, d plugin.ResourceDefinition) Package {
	t.Helper()
	definition, got, err := instance.ReadResource(t.Context(), d.Ref)
	if err != nil || !reflect.DeepEqual(d, definition) {
		t.Fatal(definition, err)
	}
	var files []File
	for _, f := range got {
		files = append(files, File{Path: f.Path, Data: f.Data, Executable: f.Executable})
	}
	origin := protocol.PluginResourceOrigin{Plugin: protocol.ResourceRef{ID: p.Info.ID, Version: p.Info.Version}, DefinitionRef: d.Ref}
	source := "plugin://" + p.Info.ID + "/" + d.Kind + "/" + d.Name
	child, err := BuildPluginResourcePackage(source, origin, d, files)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func TestPluginResourceReadAndImmutablePackage(t *testing.T) {
	p, instance := readPluginResourceFixture(t, "")
	for _, d := range p.Plugin.Resources {
		child := buildChildFixture(t, p, instance, d)
		if child.Info.Kind != d.Kind || child.Info.Name != d.Name || child.Info.PluginOrigin.Plugin.Version != p.Info.Version {
			t.Fatal(child.Info)
		}
		// A package can be restored with the plugin offline and without source I/O.
		raw, _ := json.Marshal(child)
		var saved Package
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		restored, err := BuildPluginResourcePackage(saved.Info.Source, *saved.Info.PluginOrigin, *saved.PluginResource, saved.Files)
		if err != nil || !reflect.DeepEqual(child, restored) {
			t.Fatal(restored.Info, err)
		}
		content, err := restored.Content("")
		if err != nil || len(content.Entries) != child.Info.Files || !strings.Contains(content.Text, child.Instructions) || !reflect.DeepEqual(content.PluginOrigin, child.Info.PluginOrigin) {
			t.Fatal(content, err)
		}
		if d.Kind == "skill" {
			script, err := child.Content("scripts/run.sh")
			if err != nil || script.Text != "#!/bin/sh\nexit 99\n" {
				t.Fatal(script, err)
			}
		}
		before := child.Info.Version
		origin := *child.Info.PluginOrigin
		origin.Plugin.Version = strings.Repeat("a", 64)
		changed, err := BuildPluginResourcePackage(child.Info.Source, origin, *child.PluginResource, child.Files)
		if err != nil || changed.Info.Version == before {
			t.Fatal("origin not versioned", err)
		}
		// No mutable slices from a caller become part of the returned package.
		saved.Files[0].Data[0] = '!'
		saved.PluginResource.Files[0].Path = "changed"
		if !reflect.DeepEqual(child, restored) {
			t.Fatal("aliased retained bytes or inventory")
		}
	}
	instance.Close()
}

func TestPluginResourceRejectsMismatchedBytes(t *testing.T) {
	for _, mode := range []string{"resource-corrupt", "resource-wrong-ref", "resource-wrong-path", "resource-executable", "resource-null-executable", "resource-missing", "resource-frontmatter"} {
		t.Run(mode, func(t *testing.T) {
			p, instance := readPluginResourceFixture(t, mode)
			d := p.Plugin.Resources[0]
			definition, files, err := instance.ReadResource(t.Context(), d.Ref)
			if mode == "resource-frontmatter" {
				if err != nil {
					t.Fatal(err)
				}
				var data []File
				for _, f := range files {
					data = append(data, File{Path: f.Path, Data: f.Data, Executable: f.Executable})
				}
				_, err = BuildPluginResourcePackage("plugin://"+p.Info.ID+"/prompt/"+d.Name, protocol.PluginResourceOrigin{Plugin: protocol.ResourceRef{ID: p.Info.ID, Version: p.Info.Version}, DefinitionRef: d.Ref}, definition, data)
			}
			requirePluginFailure(t, err, "invalid_resource")
		})
	}
}

func TestPluginResourceFullEightMiBBundleUsesBoundedFrames(t *testing.T) {
	p, instance := readPluginResourceFixture(t, "resource-large")
	child := buildChildFixture(t, p, instance, p.Plugin.Resources[1])
	if child.Info.Bytes != protocol.MaxResourceBytes {
		t.Fatal(child.Info.Bytes)
	}
	if _, _, err := instance.ReadResource(t.Context(), "stale"); err == nil {
		t.Fatal("unknown ref accepted")
	}
}

func TestPluginResourceCancelledReadDrainsLateReply(t *testing.T) {
	release := t.TempDir() + "/release"
	t.Setenv("ZENDE_TEST_PLUGIN_RELEASE", release)
	p, instance := readPluginResourceFixture(t, "resource-block")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, _, err := instance.ReadResource(ctx, p.Plugin.Resources[0].Ref)
	if !errors.Is(err, context.DeadlineExceeded) || instance.Info().State != "ready" {
		t.Fatal(instance.Info(), err)
	}
	if err := os.WriteFile(release, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	// The abandoned reply keeps its original RPC ID and cannot poison this read.
	if _, _, err := instance.ReadResource(t.Context(), p.Plugin.Resources[1].Ref); err != nil || instance.Info().State != "ready" {
		t.Fatal(instance.Info(), err)
	}
}

func TestPluginResourceIdentityAndInventoryValidation(t *testing.T) {
	p, instance := readPluginResourceFixture(t, "")
	child := buildChildFixture(t, p, instance, p.Plugin.Resources[1])
	for _, source := range []string{"plugin://bad/skill/name", "plugin://" + p.Info.ID + "/tool/name", child.Info.Source + "/SKILL.md", child.Info.Source + "?version=x", strings.Replace(child.Info.Source, "fixture-skill", "../escape", 1)} {
		if _, err := Identity("skill", source); err == nil {
			t.Fatal("bad source", source)
		}
	}
	if _, err := Identity("prompt", child.Info.Source); err == nil {
		t.Fatal("kind mismatch accepted")
	}
	if _, err := Prepare(t.Context(), "skill", child.Info.Source); err == nil {
		t.Fatal("plugin source treated as filesystem")
	}
	for _, files := range [][]File{child.Files[:1], append(child.Files, child.Files[0]), {{Path: "SKILL.md", Data: []byte("changed")}, child.Files[1]}} {
		if _, err := BuildPluginResourcePackage(child.Info.Source, *child.Info.PluginOrigin, *child.PluginResource, files); err == nil {
			t.Fatal("bad inventory accepted")
		}
	}
}
