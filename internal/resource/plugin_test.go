package resource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/plugin"
)

func TestResourcePluginProcessHelper(t *testing.T) {
	if os.Getenv("ZENDE_RESOURCE_PLUGIN_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     string                  `json:"id"`
			Method string                  `json:"method"`
			Params plugin.InitializeParams `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "plugin.shutdown" {
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"result\":null}\n", req.ID)
			os.Exit(0)
		}
		if req.Method != "plugin.initialize" {
			os.Exit(3)
		}
		if source := os.Getenv("ZENDE_RESOURCE_PLUGIN_REWRITE"); source != "" {
			file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				os.Exit(4)
			}
			_, err = file.WriteString("\n")
			file.Close()
			if err != nil {
				os.Exit(5)
			}
		}
		fmt.Fprintln(os.Stderr, "received "+os.Getenv("PLUGIN_CREDENTIAL"))
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": plugin.InitializeResult{
			ProtocolVersion: 1, Plugin: req.Params.Plugin, Capabilities: req.Params.Capabilities,
		}}
		if json.NewEncoder(os.Stdout).Encode(reply) != nil {
			os.Exit(6)
		}
	}
	os.Exit(0)
}

func pluginFixture(t *testing.T) (plugin.Manifest, string) {
	t.Helper()
	t.Setenv("ZENDE_RESOURCE_PLUGIN_HELPER", "1")
	t.Setenv("ZENDE_RESOURCE_PLUGIN_REWRITE", "")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := plugin.Manifest{SchemaVersion: 1, ID: "resource-fixture", Version: "v1", ProtocolVersions: []int{1},
		Command: binary, Args: []string{"-test.run=^TestResourcePluginProcessHelper$"}, Capabilities: []string{"tools"}}
	return manifest, filepath.Join(t.TempDir(), "plugin.json")
}

func writePluginFixture(t *testing.T, path string, manifest plugin.Manifest) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func requirePluginFailure(t *testing.T, err error, kind string) {
	t.Helper()
	var failure *protocol.Error
	if !errors.As(err, &failure) || failure.Data.Kind != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func TestParsePluginManifest(t *testing.T) {
	const valid = "schema_version: 1\nid: example\nversion: '1'\nprotocol_versions: [1]\ncommand: /bin/example\ncapabilities: [tools, commands]\nenv:\n  KEY: env:PLUGIN_KEY\n"
	manifest, err := ParsePluginManifest([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest)
	jsonManifest, err := ParsePluginManifest(raw)
	if err != nil || manifest.Digest() != jsonManifest.Digest() {
		t.Fatal(jsonManifest, err)
	}
	for name, data := range map[string]string{
		"empty": "", "sequence": "[]", "duplicate": valid + "id: other\n", "unknown": valid + "extra: value\n",
		"numeric-version": strings.Replace(valid, "version: '1'", "version: 1", 1),
		"quoted-schema":   strings.Replace(valid, "schema_version: 1", "schema_version: '1'", 1),
		"boolean":         strings.Replace(valid, "version: '1'", "version: true", 1),
		"null":            valid + "args: null\n", "multiple": valid + "---\n{}\n", "anchor": valid + "args: &args [one]\n",
		"alias": valid + "args: *missing\n", "literal-credential": strings.Replace(valid, "env:PLUGIN_KEY", "secret-value", 1),
		"utf8": valid + string([]byte{255}), "nul": valid + "\x00", "limit": strings.Repeat(" ", plugin.MaxManifestBytes+1),
		"schema": strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePluginManifest([]byte(data))
			requirePluginFailure(t, err, "invalid_resource")
		})
	}
}

func TestPreparePluginResolvesOnlyAtProcessBoundary(t *testing.T) {
	for _, kind := range []string{"env", "file"} {
		t.Run(kind, func(t *testing.T) {
			manifest, source := pluginFixture(t)
			const secret = "fixture-resolved-credential-do-not-save"
			ref := "env:PLUGIN_SOURCE"
			if kind == "file" {
				path := filepath.Join(filepath.Dir(source), "credential")
				if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				ref = "file:" + path
			}
			manifest.Env = map[string]string{"PLUGIN_CREDENTIAL": ref}
			raw := writePluginFixture(t, source, manifest)
			prepared, instance, err := PreparePlugin(t.Context(), source, func(name string) string {
				if name != "PLUGIN_SOURCE" {
					t.Fatalf("unexpected reference %q", name)
				}
				return secret
			})
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			if instance.Info().State != "ready" || prepared.Version != manifest.Digest() || !bytes.Equal(prepared.File, raw) {
				t.Fatal(prepared, instance.Info())
			}
			instance.Close()
			encoded, err := json.Marshal([]any{prepared, instance.Info(), instance.Diagnostics()})
			if err != nil || bytes.Contains(encoded, []byte(secret)) || !strings.Contains(instance.Diagnostics().Text, "[REDACTED]") {
				t.Fatal("resolved credential escaped process boundary or diagnostic missing", err)
			}
		})
	}
}

func TestPreparePluginFailuresNeverYieldPreparedPackage(t *testing.T) {
	for _, kind := range []string{"relative", "missing", "directory", "fifo", "oversized", "missing-credential", "credential-fifo", "executable", "version", "changed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			manifest, source := pluginFixture(t)
			want := "invalid_resource"
			switch kind {
			case "missing-credential":
				manifest.Env = map[string]string{"KEY": "env:MISSING"}
				want = "unavailable"
			case "credential-fifo":
				path := filepath.Join(filepath.Dir(source), "credential-pipe")
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
				manifest.Env = map[string]string{"KEY": "file:" + path}
				want = "unavailable"
			case "executable":
				manifest.Command = filepath.Join(filepath.Dir(source), "missing-binary")
				want = "unavailable"
			case "version":
				manifest.ProtocolVersions = []int{2}
				want = "version_mismatch"
			case "changed":
				t.Setenv("ZENDE_RESOURCE_PLUGIN_REWRITE", source)
				want = "resource_changed"
			}
			writePluginFixture(t, source, manifest)
			switch kind {
			case "relative":
				source = "plugin.json"
			case "missing":
				source += ".missing"
			case "directory":
				source = filepath.Dir(source)
			case "fifo":
				source += ".fifo"
				if err := syscall.Mkfifo(source, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(source, bytes.Repeat([]byte{' '}, plugin.MaxManifestBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
				want = "resource_limit"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			prepared, instance, err := PreparePlugin(ctx, source, func(string) string { return "" })
			if kind == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				requirePluginFailure(t, err, want)
			}
			if prepared.Source != "" || len(prepared.File) != 0 || prepared.Version != "" {
				t.Fatal("failed candidate published", prepared)
			}
			if instance != nil {
				select {
				case <-instance.Closed():
				default:
					t.Fatal("failed candidate still running")
				}
				if syscall.Kill(instance.Info().PID, 0) != syscall.ESRCH {
					t.Fatal("failed candidate was not reaped")
				}
			} else if kind == "changed" {
				t.Fatal("initialization did not run")
			}
		})
	}
}
