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
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestResourceCLIUsesDurableVersionedCore(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-resource-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	paths, err := (&app{profile: "resources"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
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
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "resources"}, args...), nil, &out, &diagnostics, BuildInfo{})
		if code != 0 || diagnostics.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostics.String())
		}
		return out.Bytes()
	}
	var created protocol.Accepted
	if err := json.Unmarshal(run("agent", "create", "--cwd", dir), &created); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, "中文 skill")
	if err := os.Mkdir(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(skillDir, "SKILL.md")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(source, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	v1 := "---\nname: local-review\ndescription: 中文 review\n---\nOriginal complete instructions.\n"
	write(v1)
	register := []string{"--request-id", "register", "resource", "register", created.AgentID, "中文 skill"}
	first := run(register...)
	var accepted protocol.Accepted
	if err := json.Unmarshal(first, &accepted); err != nil || accepted.Resource == nil {
		t.Fatal(accepted, err)
	}
	id, version := accepted.Resource.ID, accepted.Resource.Version
	write(strings.Replace(v1, "Original", "Updated", 1))
	if !bytes.Equal(first, run(register...)) {
		t.Fatal("register retry reread changed source")
	}
	refresh := []string{"--request-id", "refresh", "resource", "refresh", created.AgentID, id}
	updated := run(refresh...)
	var refreshed protocol.Accepted
	if err := json.Unmarshal(updated, &refreshed); err != nil || refreshed.Resource == nil || refreshed.Resource.Version == version {
		t.Fatal(refreshed, err)
	}
	var content protocol.ResourceContent
	if err := json.Unmarshal(run("resource", "read", created.AgentID, id, "--version", version), &content); err != nil || content.Text != v1 {
		t.Fatal(content, err)
	}
	var page protocol.ResourcePage
	if err := json.Unmarshal(run("resource", "list", created.AgentID, "--query", "中文", "--limit", "1"), &page); err != nil || len(page.Items) != 1 || page.Items[0].Version != refreshed.Resource.Version {
		t.Fatal(page, err)
	}
	// Invalid refresh leaves the old ready publication, without a false receipt.
	write("---\nname: partial")
	var out, diagnostics bytes.Buffer
	code := Run(ctx, []string{"--profile", "resources", "--request-id", "bad", "resource", "refresh", created.AgentID, id}, nil, &out, &diagnostics, BuildInfo{})
	if code != 1 || out.Len() != 0 || diagnostics.Len() == 0 {
		t.Fatal(code, out.String(), diagnostics.String())
	}
	var status protocol.ResourceStatus
	if err := json.Unmarshal(run("resource", "status", created.AgentID, id), &status); err != nil || status.State != "ready" || status.Version != refreshed.Resource.Version {
		t.Fatal(status, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(updated, run(refresh...)) || !bytes.Equal(first, run(register...)) {
		t.Fatal("retry required source after acceptance")
	}
	if err := json.Unmarshal(run("resource", "read", created.AgentID, id), &content); err != nil || !strings.Contains(content.Text, "Updated") {
		t.Fatal(content, err)
	}
	for _, action := range []string{"register", "refresh", "status", "read", "list"} {
		if !bytes.Contains(run("resource", action, "--help"), []byte("Usage:")) {
			t.Fatal("missing help", action)
		}
	}
	// A separate profile cannot observe this profile's SQLite catalog.
	otherPaths, err := (&app{profile: "other"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(otherPaths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Resources(ctx, protocol.ResourceList{AgentID: created.AgentID}); err == nil {
		t.Fatal("profile catalog leaked")
	}
}

func TestResourcePromptHintsMatchRegisteredCLI(t *testing.T) {
	root := (&app{}).command()
	for _, hint := range resourceHints("/binary path/zende", "current-profile") {
		parts := strings.Split(hint.Name, ".")
		cmd, remaining, err := root.Find(parts)
		if err != nil || len(remaining) != 0 || cmd.Name() != parts[1] || cmd.RunE == nil {
			t.Fatal(hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "'/binary path/zende' --profile 'current-profile'") {
			t.Fatal("unbound CLI hint", hint.Usage)
		}
	}
}
