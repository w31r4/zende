package resource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompositionEveryLayer(t *testing.T) {
	layers := []Layer{}
	for i, name := range []string{"user", "project", "local", "environment", "cli", "task"} {
		layers = append(layers, Layer{name, []byte("cwd: /" + name + "\nresource_dirs: [ /" + name + " ]\nlimits:\n  agents: " + string(rune('1'+i)) + "\n")})
		got, err := Compose(Defaults("/base"), layers...)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.Cwd != "/"+name || got.Sources["/cwd"] != name || got.Config.Limits.Agents != i+1 || got.Config.Limits.Depth != 4 || len(got.Config.ResourceDirs) != 1 || got.Config.ResourceDirs[0] != "/"+name {
			t.Fatalf("layer %s: %+v", name, got)
		}
	}
}

func TestInvalidConfigurationDoesNotExposeSecrets(t *testing.T) {
	for _, input := range []string{
		"limits: { agents: nope }", "limits: { agents: 0 }", "unexpected: TOPSECRET", "cwd: true", "cwd: [ /tmp ]", "cwd: relative",
		"cwd: /one\ncwd: /two", "---\ncwd: /one\n---\ncwd: /two", "connections: { demo: { credential: TOPSECRET } }", "model: null", "resource_dirs: [null]",
		"connections: { demo: { protocol: responses, base_url: 'https://x/?key=TOPSECRET', models: [] } }",
	} {
		t.Run(input, func(t *testing.T) {
			_, err := Compose(Defaults("/tmp"), Layer{"test", []byte(input)})
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatal("secret in diagnostic")
			}
		})
	}
}

func TestConfiguredModelsAndCredentials(t *testing.T) {
	input := `connections:
  private:
    label: My endpoint
    vendor: kimi
    protocol: chat-completions
    base_url: https://example.invalid/v1
    credential: env:DEMO_KEY
    models:
      - id: org/model
        context_window: 32000
        max_output: 4096
        efforts: [low, high]
model: private/org/model
effort: high
`
	e, err := Compose(Defaults("/tmp"), Layer{"test", []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ResolveCredential(e.Config.Connections["private"].Credential, func(string) string { return "TOPSECRET" })
	if err != nil || secret != "TOPSECRET" {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), secret) {
		t.Fatal("resolved credential persisted")
	}
	if _, err := ResolveCredential("env:DEMO_KEY", func(string) string { return "" }); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := Compose(Defaults("/tmp"), Layer{"test", []byte(input)}, Layer{"override", []byte("effort: fictional")}); err == nil {
		t.Fatal("unsupported effort accepted")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte("FILESECRET\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := ResolveCredential("file:"+file, os.Getenv); err != nil || value != "FILESECRET" {
		t.Fatal(value, err)
	}
}

func TestPathsIsolationAndLoad(t *testing.T) {
	root := t.TempDir()
	env := func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return filepath.Join(root, "cfg")
		case "XDG_STATE_HOME":
			return filepath.Join(root, "state")
		case "XDG_RUNTIME_DIR":
			return "/tmp/zende-test"
		}
		return ""
	}
	a, err := ResolvePaths("a", root, env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolvePaths("b", root, env)
	if err != nil {
		t.Fatal(err)
	}
	if a.Database == b.Database || a.Socket == b.Socket || a.Config == b.Config {
		t.Fatal("profiles share paths")
	}
	for _, name := range []string{"", "../escape", "a/b", "a b"} {
		if _, err := ResolvePaths(name, root, env); err == nil {
			t.Fatal(name)
		}
	}
	if _, err := os.Stat(a.Config); !os.IsNotExist(err) {
		t.Fatal("resolution wrote config")
	}
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, ".zende"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.Config, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(a.Config, "config.yaml"): "limits: { agents: 1 }", filepath.Join(project, ".zende/config.yaml"): "limits: { agents: 2 }", filepath.Join(project, ".zende/config.local.yaml"): "limits: { agents: 3 }"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Load(a, project, func(k string) string {
		if k == "ZENDE_CWD" {
			return "/environment"
		}
		return ""
	}, []byte("cwd: /cli"), []byte("cwd: /task"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Config.Limits.Agents != 3 || e.Config.Cwd != "/task" || e.Sources["/limits/agents"] != filepath.Join(project, ".zende/config.local.yaml") {
		t.Fatal(e)
	}
	isolated, err := Load(b, root, func(string) string { return "" }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Config.Limits.Agents != 8 || len(isolated.Config.Connections) != 0 {
		t.Fatal(isolated)
	}
}
