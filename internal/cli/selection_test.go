package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestSelectionCLIUsesPersistedAcceptance(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-selection-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	paths, err := (&app{profile: "selection"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := resource.Defaults(dir)
	cfg.Model, cfg.Effort = "local/model", "high"
	cfg.Connections["local"] = resource.Connection{Protocol: "responses", BaseURL: "https://test.invalid/v1", Credential: "env:UNRESOLVED_TEST_KEY", Models: []resource.ModelConfig{{ID: "model", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"low", "high"}}}}
	resolver, err := provider.New(cfg, func(string) string { t.Error("selection resolved credentials"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	c := runtime.New(s, resource.Effective{Config: cfg})
	c.Catalog, c.RestoreModels = resolver, resolver
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	run := func(args ...string) []byte {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "selection"}, args...), nil, &out, &diagnostic, BuildInfo{})
		if code != 0 || diagnostic.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostic.String())
		}
		return out.Bytes()
	}
	var created protocol.Accepted
	if err := json.Unmarshal(run("agent", "create", "--cwd", dir, "kept input"), &created); err != nil {
		t.Fatal(err)
	}
	before := run("agent", "history", created.AgentID)
	var initial protocol.AgentSelection
	if err := json.Unmarshal(run("agent", "selection", created.AgentID), &initial); err != nil || initial.Revision != 0 {
		t.Fatal(initial, err)
	}
	if !bytes.Equal(before, run("agent", "history", created.AgentID)) {
		t.Fatal("selection query mutated history")
	}
	target := filepath.Join(dir, "中文 folder")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	var catalog protocol.ConnectionPage
	if err := json.Unmarshal(run("model", "connections"), &catalog); err != nil {
		t.Fatal(err)
	}
	args := []string{"--request-id", "select-once", "agent", "select", created.AgentID, "--cwd", "中文 folder", "--model", "local/model", "--effort", "", "--catalog-version", catalog.Version, "--expected-revision", "0"}
	data := run(args...)
	var accepted protocol.Accepted
	if err := json.Unmarshal(data, &accepted); err != nil || accepted.Selection == nil || accepted.Selection.Cwd != target || accepted.Selection.Effort != "" {
		t.Fatal(accepted, err)
	}
	if bytes.Contains(data, []byte("UNRESOLVED_TEST_KEY")) || bytes.Contains(data, []byte("test.invalid")) {
		t.Fatal("selection leaked binding internals")
	}
	// Retry does not revisit the now-invalid relative path or obsolete revision.
	if again := run(args...); !bytes.Equal(again, data) {
		t.Fatal("retry changed acceptance", string(again))
	}
	var saved protocol.AgentSelection
	if err := json.Unmarshal(run("agent", "selection", created.AgentID), &saved); err != nil || saved != *accepted.Selection {
		t.Fatal(saved, err)
	}
	history := run("agent", "history", created.AgentID)
	for _, invalid := range [][]string{
		{"agent", "select", created.AgentID},
		{"agent", "select", created.AgentID, "--effort", "high"},
		{"agent", "select", created.AgentID, "--cwd", ".", "--expected-revision", "0"},
		{"agent", "select", created.AgentID, "--cwd", "missing"},
		{"agent", "select", created.AgentID, "--model", "local/model", "--effort", "ultra"},
		{"agent", "select", created.AgentID, "--model", "local/model", "--catalog-version", "stale"},
		{"agent", "select", created.AgentID, "--model", "unconfigured/model"},
	} {
		var out, diagnostic bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "selection"}, invalid...), nil, &out, &diagnostic, BuildInfo{}); code == 0 || out.Len() != 0 || diagnostic.Len() == 0 {
			t.Fatal(invalid, code, diagnostic.String())
		}
	}
	if !bytes.Equal(history, run("agent", "history", created.AgentID)) {
		t.Fatal("rejected selection changed state")
	}
	for _, name := range []string{"selection", "select"} {
		if !bytes.Contains(run("agent", name, "--help"), []byte("Usage:")) {
			t.Fatal("missing CLI help", name)
		}
	}
	for _, method := range []string{"agent.selection", "agent.select"} {
		req, _ := protocol.NewRequest(method, "invalid-field", map[string]string{"agent_id": created.AgentID, "unknown": "field"})
		if _, err := c.Handle(ctx, req); err == nil {
			t.Fatal("unknown field accepted", method)
		}
	}
}

func TestSelectionPromptHintsMatchCLI(t *testing.T) {
	root := (&app{}).command()
	for _, hint := range selectionHints("/binary path/zende", "bound-profile") {
		parts := strings.Split(hint.Name, ".")
		cmd, remaining, err := root.Find(parts)
		if err != nil || len(remaining) != 0 || cmd.Name() != parts[1] || cmd.RunE == nil {
			t.Fatal(hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "'/binary path/zende' --profile 'bound-profile'") {
			t.Fatal(hint.Usage)
		}
	}
}
