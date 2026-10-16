package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"zende.local/harness/internal/protocol"
)

func configFixture(t *testing.T) (Paths, string, func(string, string)) {
	t.Helper()
	root := t.TempDir()
	paths := Paths{Config: filepath.Join(root, "profile")}
	cwd := filepath.Join(root, "project")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return paths, cwd, write
}

func noConfigEnvironment(string) string { return "" }

func requireConfigError(t *testing.T, err error, kind string) {
	t.Helper()
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Data.Kind != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func TestPreparedConfigProvenanceAndImmutableSnapshot(t *testing.T) {
	paths, cwd, write := configFixture(t)
	user := filepath.Join(paths.Config, "config.yaml")
	project := filepath.Join(cwd, ".zende", "config.yaml")
	local := filepath.Join(cwd, ".zende", "config.local.yaml")
	write(user, `connections:
  demo:
    protocol: responses
    base_url: https://example.invalid/v1
    credential: TOPSECRET
    models: [{id: test, context_window: 32000, max_output: 4096, efforts: [high]}]
model: demo/test
limits: {agents: 1}
`)
	write(project, "connections: {demo: {credential: 'env:TEST_CONFIG_KEY'}}\nlimits: {agents: 2}")
	write(local, "limits: {agents: 3}\nresource_dirs: [/resources]")
	env := func(k string) string {
		switch k {
		case "ZENDE_CWD":
			return "/environment"
		case "ZENDE_MODEL", "ZENDE_EFFORT", "ZENDE_SHELL":
			return ""
		default:
			t.Fatalf("preparation resolved credential or unrelated variable %s", k)
			return "TOPSECRET"
		}
	}
	cli, task := []byte("cwd: /cli"), []byte("cwd: /task")
	p, err := PrepareConfig(context.Background(), paths, cwd, env, cli, task)
	if err != nil {
		t.Fatal(err)
	}
	original := p.Snapshot()
	if len(original.Version) != 64 || original.Effective.Config.Cwd != "/task" || original.Effective.Config.Limits.Agents != 3 || original.Effective.Sources["/cwd"] != "task" || original.Effective.Sources["/limits/agents"] != local || original.Effective.Sources["/connections/demo/credential"] != project {
		t.Fatalf("invalid snapshot: %+v", original)
	}
	for i, source := range []string{user, project, local} {
		doc := original.Documents[i]
		if doc.Source != source || !doc.Present || doc.Bytes == 0 || len(doc.SHA256) != 64 {
			t.Fatalf("invalid source: %+v", doc)
		}
	}
	raw, err := json.Marshal(original)
	if err != nil || bytes.Contains(raw, []byte("TOPSECRET")) || !bytes.Contains(raw, []byte("env:TEST_CONFIG_KEY")) {
		t.Fatalf("snapshot must preserve only credential reference: %v", err)
	}
	unchanged, err := PrepareConfig(context.Background(), paths, cwd, env, cli, task)
	if err != nil || !reflect.DeepEqual(unchanged.Snapshot(), original) {
		t.Fatalf("identical preparation changed version: %v", err)
	}
	// Returned maps, nested slices, documents and caller-owned overrides must
	// not mutate the candidate or race concurrent snapshot readers.
	copy(cli, "cwd: /bad")
	copy(task, "cwd: /oops")
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			copy := p.Snapshot()
			copy.Effective.Config.Connections["demo"].Models[0].Efforts[0] = "other"
			delete(copy.Effective.Config.Connections, "demo")
			copy.Effective.Config.ResourceDirs[0] = "/other"
			copy.Effective.Sources["/cwd"] = "other"
			copy.Documents[0].SHA256 = "other"
		})
	}
	readers.Wait()
	if !reflect.DeepEqual(p.Snapshot(), original) {
		t.Fatal("exported snapshot mutated prepared state")
	}
	if err := p.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedConfigDetectsSourceCreationRemovalAndReplacement(t *testing.T) {
	for _, which := range []string{"user", "project", "local"} {
		t.Run(which, func(t *testing.T) {
			paths, cwd, write := configFixture(t)
			source := map[string]string{"user": filepath.Join(paths.Config, "config.yaml"), "project": filepath.Join(cwd, ".zende", "config.yaml"), "local": filepath.Join(cwd, ".zende", "config.local.yaml")}[which]
			prepare := func() *PreparedConfig {
				t.Helper()
				p, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			absent := prepare()
			if _, err := os.Stat(paths.Config); !os.IsNotExist(err) {
				t.Fatalf("preparation created a missing directory: %v", err)
			}
			write(source, "")
			requireConfigError(t, absent.Verify(context.Background()), "resource_changed")
			empty := prepare()
			if empty.Snapshot().Version == absent.Snapshot().Version || !reflect.DeepEqual(empty.Snapshot().Effective, absent.Snapshot().Effective) {
				t.Fatal("empty file and absent file need different source versions, same values")
			}
			write(source, "limits: {agents: 3}")
			requireConfigError(t, empty.Verify(context.Background()), "resource_changed")
			valid := prepare()
			write(source, "limits: {agents: 3}\n# a comment")
			requireConfigError(t, valid.Verify(context.Background()), "resource_changed")
			comment := prepare()
			if valid.Snapshot().Version == comment.Snapshot().Version || !reflect.DeepEqual(valid.Snapshot().Effective, comment.Snapshot().Effective) {
				t.Fatal("source hashes must include comments without changing effective values")
			}
			// Model an atomic rename of a completed replacement.
			write(source+".new", "limits: {agents: 5}")
			if err := os.Rename(source+".new", source); err != nil {
				t.Fatal(err)
			}
			requireConfigError(t, comment.Verify(context.Background()), "resource_changed")
			replacement := prepare()
			if replacement.Snapshot().Effective.Config.Limits.Agents != 5 {
				t.Fatal("replacement values missing")
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			requireConfigError(t, replacement.Verify(context.Background()), "resource_changed")
			if prepare().Snapshot().Version != absent.Snapshot().Version {
				t.Fatal("restored source contents should produce original version")
			}
		})
	}
}

func TestPreparedConfigRejectsPartialUpdateAndKeepsCandidate(t *testing.T) {
	paths, cwd, write := configFixture(t)
	source := filepath.Join(paths.Config, "config.yaml")
	write(source, "limits: {agents: 2}")
	p, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := p.Snapshot()
	for _, body := range []string{"limits: {agents:", "limits: {agents: 0}", "unknown: TOPSECRET", "cwd: null"} {
		write(source, body)
		_, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil)
		requireConfigError(t, err, "invalid_config")
		if strings.Contains(err.Error(), "TOPSECRET") || !reflect.DeepEqual(p.Snapshot(), original) {
			t.Fatal("failed update changed previous candidate or leaked source text")
		}
		requireConfigError(t, p.Verify(context.Background()), "resource_changed")
	}
	write(source, "limits: {agents: 4}")
	next, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil)
	if err != nil || next.Snapshot().Effective.Config.Limits.Agents != 4 || next.Snapshot().Version == original.Version {
		t.Fatalf("completed replacement was not prepared: %v", err)
	}
}

func TestPreparedConfigEnvironmentCancellationAndRoots(t *testing.T) {
	paths, cwd, _ := configFixture(t)
	value := "/one"
	env := func(k string) string {
		if k == "ZENDE_CWD" {
			return value
		}
		return ""
	}
	p, err := PrepareConfig(context.Background(), paths, cwd, env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	value = "/two"
	requireConfigError(t, p.Verify(context.Background()), "resource_changed")
	for _, value := range []string{strings.Repeat("X", MaxConfigLayerBytes+1), "\xff"} {
		_, err := PrepareConfig(context.Background(), paths, cwd, func(string) string { return value }, nil, nil)
		requireConfigError(t, err, "invalid_config")
	}
	for _, key := range []string{"ZENDE_CWD", "ZENDE_MODEL", "ZENDE_EFFORT", "ZENDE_SHELL"} {
		t.Setenv(key, "")
	}
	t.Setenv("ZENDE_CWD", "/environment")
	p, err = PrepareConfig(context.Background(), paths, cwd, nil, nil, nil)
	if err != nil || p.Snapshot().Effective.Config.Cwd != "/environment" {
		t.Fatalf("nil environment did not read process environment: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareConfig(ctx, paths, cwd, func(string) string { t.Fatal("canceled preparation read environment"); return "" }, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := p.Verify(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, invalid := range []string{"relative", "/tmp/../config", "/tmp/\x00", "/tmp/\xff", "/" + strings.Repeat("a", 4096)} {
		_, err := PrepareConfig(context.Background(), Paths{Config: invalid}, cwd, noConfigEnvironment, nil, nil)
		requireConfigError(t, err, "invalid_config")
		_, err = PrepareConfig(context.Background(), paths, invalid, noConfigEnvironment, nil, nil)
		requireConfigError(t, err, "invalid_config")
	}
}

func TestPreparedConfigProfileIsolationAndFileTypes(t *testing.T) {
	paths, cwd, write := configFixture(t)
	source := filepath.Join(paths.Config, "config.yaml")
	write(source, "limits: {agents: 2}")
	otherPaths := Paths{Config: filepath.Join(filepath.Dir(paths.Config), "other")}
	other, err := PrepareConfig(context.Background(), otherPaths, cwd, noConfigEnvironment, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(source, "limits: {agents:")
	if err := other.Verify(context.Background()); err != nil || other.Snapshot().Effective.Config.Limits.Agents != 8 {
		t.Fatalf("profile configuration leaked: %v", err)
	}
	for _, kind := range []string{"directory", "fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "config.yaml")
			switch kind {
			case "directory":
				if err := os.Mkdir(file, 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(file, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				write(filepath.Join(root, "target"), "limits: {agents: 3}")
				if err := os.Symlink("target", file); err != nil {
					t.Fatal(err)
				}
			}
			p, err := PrepareConfig(context.Background(), Paths{Config: root}, cwd, noConfigEnvironment, nil, nil)
			if kind != "symlink" {
				requireConfigError(t, err, "invalid_config")
			} else if err != nil || p.Snapshot().Effective.Config.Limits.Agents != 3 {
				t.Fatalf("regular symlink target should work: %v", err)
			}
		})
	}
}

func TestConfigurationBudgetsAndYAMLStructure(t *testing.T) {
	paths, cwd, write := configFixture(t)
	oversized := []byte(strings.Repeat(" ", MaxConfigLayerBytes+1))
	for _, task := range []bool{false, true} {
		cli, override := oversized, []byte(nil)
		if task {
			cli, override = nil, oversized
		}
		_, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, cli, override)
		requireConfigError(t, err, "resource_limit")
	}
	source := filepath.Join(paths.Config, "config.yaml")
	write(source, string(oversized))
	_, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil)
	requireConfigError(t, err, "resource_limit")
	bounded := "cwd: /ok\n#" + strings.Repeat(" ", MaxConfigLayerBytes-len("cwd: /ok\n#"))
	write(source, bounded)
	if _, err := PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, nil, nil); err != nil {
		t.Fatalf("exact layer budget rejected: %v", err)
	}
	layers := []Layer{}
	for range 4 {
		layers = append(layers, Layer{"bounded", []byte(bounded)})
	}
	_, err = Compose(Defaults(cwd), layers...)
	requireConfigError(t, err, "resource_limit")
	write(filepath.Join(cwd, ".zende", "config.yaml"), bounded)
	write(filepath.Join(cwd, ".zende", "config.local.yaml"), bounded)
	_, err = PrepareConfig(context.Background(), paths, cwd, noConfigEnvironment, []byte(bounded), nil)
	requireConfigError(t, err, "resource_limit")
	defaults := Defaults("/" + strings.Repeat("x", MaxConfigBytes))
	_, err = Compose(defaults)
	requireConfigError(t, err, "resource_limit")
	_, err = Compose(Defaults(cwd), Layer{"escaped", []byte("cwd: /" + strings.Repeat("&", 720000))})
	requireConfigError(t, err, "resource_limit")
	connections := map[string]Connection{}
	for i := range 500 {
		connections[fmt.Sprintf("c%d", i)] = Connection{Protocol: "responses", BaseURL: "https://example.invalid", Models: []ModelConfig{}}
	}
	body, err := json.Marshal(map[string]any{"connections": connections})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compose(Defaults(cwd), Layer{strings.Repeat("x", 3000), body})
	requireConfigError(t, err, "resource_limit")
	for _, body := range []string{
		"resource_dirs: [&directory /tmp, *directory]",
		"limits: &limits {agents: 2}",
		"limits: !custom {agents: 2}",
		"resource_dirs: !custom [/tmp]",
		"cwd: !custom /tmp",
		"limits: {agents: 1, agents: 2}",
		"limits: {true: TOPSECRET}",
		"resource_dirs: [null]",
		"cwd: /tmp\n---\nsecret: &s [TOPSECRET, *s]",
		"resource_dirs: " + strings.Repeat("[", 65) + "TOPSECRET" + strings.Repeat("]", 65),
	} {
		if _, err := Compose(Defaults(cwd), Layer{"test", []byte(body)}); err == nil || strings.Contains(err.Error(), "TOPSECRET") {
			t.Fatalf("invalid structure accepted or leaked: %v", err)
		}
	}
}
