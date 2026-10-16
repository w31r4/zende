package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/resource/plugin/fixture"
)

func TestPluginPackageProcessHelper(t *testing.T) { fixture.RunFromEnv() }

func packagePluginFixture(t *testing.T) (plugin.Manifest, string) {
	t.Helper()
	t.Setenv("ZENDE_TEST_PLUGIN", "1")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_AUDIT", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", Command: executable, Args: []string{"-test.run=^TestPluginPackageProcessHelper$"}, ProtocolVersions: []int{1}, Capabilities: []string{"tools", "commands"}}, filepath.Join(t.TempDir(), "plugin.json")
}

func TestPluginPackageRetainsVerifiedManifestAndCatalog(t *testing.T) {
	m, source := packagePluginFixture(t)
	m.Env = map[string]string{"PLUGIN_KEY": "env:ZENDE_TEST_PLUGIN_KEY"}
	t.Setenv("ZENDE_TEST_PLUGIN_KEY", "fixture-secret-never-persist")
	raw := writePluginFixture(t, source, m)
	ctx, cancel := context.WithCancel(t.Context())
	p, instance, err := PreparePluginPackage(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	cancel() // The preparing client's lifetime does not own a ready instance.
	if instance.Info().State != "ready" || p.Plugin == nil || len(p.Plugin.Definitions) != 2 {
		t.Fatal(p, instance.Info())
	}
	saved, err := json.Marshal(p)
	if err != nil || bytes.Contains(saved, []byte("fixture-secret-never-persist")) {
		t.Fatal("persisted expanded credential", err)
	}
	var copy Package
	if err := json.Unmarshal(saved, &copy); err != nil {
		t.Fatal(err)
	}
	// Restoration uses persisted bytes even with no source or credential.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN_KEY", "")
	restored, err := BuildPluginPackage(source, copy.Files, *copy.Plugin)
	if err != nil || !reflect.DeepEqual(restored, p) {
		t.Fatal("round trip changed package", err)
	}
	content, err := p.Content("")
	if err != nil || content.Text != string(raw) || content.Path != "plugin.json" {
		t.Fatal(content, err)
	}
	copy.Files[0].Data[0] = '!'
	if _, err := BuildPluginPackage(source, copy.Files, *copy.Plugin); err == nil {
		t.Fatal("accepted corrupt manifest")
	}
	if !bytes.Equal(p.Files[0].Data, raw) {
		t.Fatal("mutated original package")
	}
}

func TestPluginPackageDiscoveryFailureAndSourceChange(t *testing.T) {
	for _, mode := range []string{"invalid", "rewrite", "stall"} {
		t.Run(mode, func(t *testing.T) {
			m, source := packagePluginFixture(t)
			writePluginFixture(t, source, m)
			t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
			if mode == "rewrite" {
				t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", source)
			}
			ctx := t.Context()
			if mode == "stall" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			p, instance, err := PreparePluginPackage(ctx, source, nil)
			if err == nil || instance != nil || p.Info.Version != "" {
				t.Fatal("failed preparation published", p, instance, err)
			}
			if mode == "rewrite" {
				requirePluginFailure(t, err, "resource_changed")
			}
			if mode == "invalid" {
				requirePluginFailure(t, err, "plugin_protocol")
			}
		})
	}
}

func TestPluginPackageExtendedCatalog(t *testing.T) {
	for _, mode := range []string{"", "invalid-provider", "invalid-resource"} {
		t.Run(mode, func(t *testing.T) {
			m, source := packagePluginFixture(t)
			m.Capabilities = append(m.Capabilities, "providers", "resources")
			t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
			writePluginFixture(t, source, m)
			p, instance, err := PreparePluginPackage(t.Context(), source, nil)
			if mode != "" {
				if err == nil || instance != nil || p.Info.Version != "" {
					t.Fatal("invalid extended catalog published", p, err)
				}
				requirePluginFailure(t, err, "plugin_protocol")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			if len(p.Plugin.Providers) != 1 || len(p.Plugin.Resources) != 1 || len(p.Plugin.Definitions) != 2 {
				t.Fatal(p.Plugin)
			}
			if p.Plugin.Providers[0].Models[0].ID != "fixture-text" || p.Plugin.Resources[0].Entry != "instructions.md" {
				t.Fatal(p.Plugin)
			}
			instance.Close()
			raw, _ := json.Marshal(p)
			var saved Package
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			restored, err := BuildPluginPackage(source, saved.Files, *saved.Plugin)
			if err != nil || !reflect.DeepEqual(restored, p) {
				t.Fatal(restored, err)
			}
			saved.Plugin.Providers[0].Models[0].MaxOutput++
			if _, err := BuildPluginPackage(source, saved.Files, *saved.Plugin); err == nil {
				t.Fatal("altered provider metadata accepted")
			}
		})
	}
}
