package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestCLIExplicitTreeStopAndLiveInspection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv(protocol.CallerEnvironment, "")
	paths, err := (&app{profile: "tree"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := process.NewManager(filepath.Join(dir, "processes"), "/bin/sh", 8, 65536)
	if err != nil {
		t.Fatal(err)
	}
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	c.Runner = m
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served, ran := make(chan error, 1), make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	go func() { ran <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
		if err := <-ran; err != nil {
			t.Error(err)
		}
	}()
	run := func(args ...string) []byte {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "tree"}, args...), nil, &out, &diagnostics, BuildInfo{}); code != 0 || diagnostics.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, out.String(), diagnostics.String())
		}
		return out.Bytes()
	}
	var parent, child protocol.Accepted
	if err := json.Unmarshal(run("agent", "create", "--cwd", dir), &parent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run("agent", "spawn", "--parent", parent.AgentID, "child queued"), &child); err != nil {
		t.Fatal(err)
	}
	run("--request-id", "single", "agent", "stop", parent.AgentID)
	if a, err := s.Agent(ctx, child.AgentID); err != nil || a.Paused {
		t.Fatal("ordinary stop expanded to descendants", a, err)
	}
	first := run("--request-id", "tree-stop", "agent", "stop", parent.AgentID, "--tree")
	if again := run("--request-id", "tree-stop", "agent", "stop", parent.AgentID, "--tree"); !bytes.Equal(first, again) {
		t.Fatal("retry changed acceptance", string(again))
	}
	var plan store.TreeStopRecord
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err := json.Unmarshal(run("operation", "inspect", "tree-stop"), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.State == "stopped" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if plan.State != "stopped" || plan.Stage != "completed" || len(plan.Members) != 2 {
		t.Fatal(plan)
	}
	if a, err := s.Agent(ctx, child.AgentID); err != nil || !a.Paused {
		t.Fatal(a, err)
	}
	for _, args := range [][]string{
		{"agent", "resume", parent.AgentID, "--tree"},
		{"agent", "stop", "--tree"},
		{"--request-id", "single", "agent", "stop", parent.AgentID, "--tree"},
	} {
		var out, diagnostics bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "tree"}, args...), nil, &out, &diagnostics, BuildInfo{}); code == 0 || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatal(args, code, out.String(), diagnostics.String())
		}
	}
	help := string(run("agent", "stop", "--help"))
	for _, text := range []string{"--tree", "Acceptance is not completion", "operation inspect ID", "background processes continue"} {
		if !strings.Contains(help, text) {
			t.Fatal("help missing", text)
		}
	}
	run("agent", "resume", parent.AgentID)
	if a, err := s.Agent(ctx, child.AgentID); err != nil || !a.Paused {
		t.Fatal("resume expanded scope", a, err)
	}
}
