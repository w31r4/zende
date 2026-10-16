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
	register := []string{"--request-id", "register", "resource", "register", created.AgentID, "中文 skill"}
	// A first failed install returns an identity even without any ready version.
	var installOut, installErrors bytes.Buffer
	code := Run(ctx, append([]string{"--profile", "resources"}, register...), nil, &installOut, &installErrors, BuildInfo{})
	var failure protocol.Error
	if code != 1 || installOut.Len() != 0 || json.Unmarshal(installErrors.Bytes(), &failure) != nil || failure.Data.ResourceID == "" || failure.Data.OperationID != "register" {
		t.Fatal(code, installOut.String(), installErrors.String())
	}
	var missing protocol.ResourceStatus
	if err := json.Unmarshal(run("resource", "status", created.AgentID, failure.Data.ResourceID), &missing); err != nil || missing.State != "failed" || missing.Version != "" || missing.Update == nil {
		t.Fatal(missing, err)
	}
	write(v1)
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
	var ready protocol.ResourceStatus
	if err := json.Unmarshal(run("resource", "status", created.AgentID, id), &ready); err != nil || ready.State != "ready" || ready.Update == nil || ready.Update.State != "ready" || ready.Version != refreshed.Resource.Version {
		t.Fatal("documented ready query failed", ready, err)
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
	code = Run(ctx, []string{"--profile", "resources", "--request-id", "bad", "resource", "refresh", created.AgentID, id}, nil, &out, &diagnostics, BuildInfo{})
	if code != 1 || out.Len() != 0 || diagnostics.Len() == 0 {
		t.Fatal(code, out.String(), diagnostics.String())
	}
	var status protocol.ResourceStatus
	if err := json.Unmarshal(run("resource", "status", created.AgentID, id), &status); err != nil || status.State != "ready" || status.Version != refreshed.Resource.Version {
		t.Fatal(status, err)
	}
	if status.Update == nil || status.Update.State != "failed" || status.Update.OperationID != "bad" || status.Update.Error == nil || status.Update.Error.Data.Kind != "invalid_resource" {
		t.Fatal("failed update was hidden by last ready version", status)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(updated, run(refresh...)) || !bytes.Equal(first, run(register...)) {
		t.Fatal("retry required source after acceptance")
	}
	var unchanged protocol.ResourceStatus
	if err := json.Unmarshal(run("resource", "status", created.AgentID, id), &unchanged); err != nil || unchanged.Update == nil || unchanged.Update.Error == nil || *unchanged.Update.Error != *status.Update.Error || unchanged.Update.AttemptID != status.Update.AttemptID {
		t.Fatal("retry of old acceptance changed latest failure", unchanged, err)
	}
	if err := json.Unmarshal(run("resource", "read", created.AgentID, id), &content); err != nil || !strings.Contains(content.Text, "Updated") {
		t.Fatal(content, err)
	}
	// CLI selection pins the original, now filesystem-independent body.
	var submitted protocol.Accepted
	if err := json.Unmarshal(run("agent", "submit", created.AgentID, "--skill", id+"@"+version), &submitted); err != nil {
		t.Fatal(err)
	}
	work, err := s.ClaimNext(ctx, created.AgentID)
	if err != nil || work == nil || work.Task.ID != submitted.TaskID || len(work.Task.Skills) != 1 || work.Task.Skills[0].Version != version {
		t.Fatal(work, err)
	}
	var asked protocol.Accepted
	if err := json.Unmarshal(run("question", "ask", created.AgentID, "--task", work.Task.ID, "Which instructions?"), &asked); err != nil {
		t.Fatal(err)
	}
	run("question", "answer", created.AgentID, asked.QuestionID, "--skill", id+"@"+version)
	if ready, err := s.QuestionBarrier(ctx, created.AgentID, work.Task.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
	messages, _, err := s.ModelContext(ctx, created.AgentID)
	if err != nil || len(messages) != 2 {
		t.Fatal(messages, err)
	}
	for _, message := range messages {
		last := message.Content[len(message.Content)-1]
		if message.Role != "user" || !strings.Contains(last.Text, "Original complete instructions.") || strings.Contains(last.Text, "Updated") {
			t.Fatal(message)
		}
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

func TestSkillFlagRequiresExactVersion(t *testing.T) {
	for _, values := range [][]string{{"name"}, {"id@latest"}, {"id@"}, {"@" + strings.Repeat("a", 64)}, make([]string, protocol.MaxInputSkills+1)} {
		if _, err := parseSkillRefs(values); err == nil {
			t.Fatal("invalid selection accepted", values)
		}
	}
	refs, err := parseSkillRefs([]string{"id@" + strings.Repeat("a", 64)})
	if err != nil || len(refs) != 1 || refs[0].ID != "id" {
		t.Fatal(refs, err)
	}
}

func TestResourcePromptHintsMatchRegisteredCLI(t *testing.T) {
	root := (&app{}).command()
	for _, hint := range resourceHints("/binary path/zende", "current-profile") {
		parts := strings.Split(hint.Name, ".")
		cmd, remaining, err := root.Find(parts)
		if err != nil || len(remaining) != 0 || cmd.Name() != parts[len(parts)-1] || cmd.RunE == nil {
			t.Fatal(hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "'/binary path/zende' --profile 'current-profile'") {
			t.Fatal("unbound CLI hint", hint.Usage)
		}
		if hint.Name == "plugin.command" {
			if cmd.Flags().Lookup("file") == nil {
				t.Fatal("missing JSON input flag")
			}
			for _, text := range []string{"command refs", "not its completion", "inspect/progress/result", "cancel", "request ID", "unknown effect"} {
				if !strings.Contains(hint.Guidance, text) {
					t.Fatal("plugin hint missing", text)
				}
			}
		}
		if hint.Name == "plugin.events.subscribe" {
			for _, name := range []string{"after", "kind"} {
				if cmd.Flags().Lookup(name) == nil {
					t.Fatal("missing subscription flag", name)
				}
			}
			for _, text := range []string{"core", "plugin", "subscription ID", "ordinary Task stop", "list/inspect/read/close", "unknown"} {
				if !strings.Contains(hint.Guidance, text) {
					t.Fatal("event hint missing", text)
				}
			}
		}
	}
}
