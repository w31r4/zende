package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestHelpVersionAndUsage(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"tui", "--help"}, {"agent", "--help"}, {"agent", "stop", "--help"}, {"agent", "resume", "--help"}, {"agent", "recovery", "--help"}, {"agent", "recover", "--help"}, {"core", "--help"}, {"--version"}, {"version"}} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut, BuildInfo{"test-version", "test-commit", "test-date"})
		if code != 0 || out.Len() == 0 || errOut.Len() != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"agent", "submit"}, nil, &out, &errOut, BuildInfo{})
	if code == 0 || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatal("invalid usage corrupted stdout", code, out.String(), errOut.String())
	}
}

func TestLongTextAndInvalidInput(t *testing.T) {
	text := strings.Repeat("中文\n", 20000)
	path := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(text))
	for _, file := range []string{path, "-"} {
		got, err := readInput(cmd, nil, file, true)
		if err != nil || got != text {
			t.Fatal(len(got), err)
		}
	}
	if _, err := readInput(cmd, []string{"text"}, path, true); err == nil {
		t.Fatal("ambiguous input accepted")
	}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", 2<<20+1)))
	if _, err := readInput(cmd, nil, "-", true); err == nil {
		t.Fatal("oversized input")
	}
	cmd.SetIn(bytes.NewReader([]byte{0xff}))
	if _, err := readInput(cmd, nil, "-", true); err == nil {
		t.Fatal("invalid UTF-8")
	}
}

func TestCLIJSONStdioAndStableRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	a := &app{profile: "cli-test"}
	p, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureState(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(p.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	server, err := protocol.Listen(p.Socket, core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	args := []string{"--profile", "cli-test", "--request-id", "cli-once", "agent", "create", "--cwd", dir, "--file", "-", "--json"}
	var out, errOut bytes.Buffer
	input := strings.Repeat("payload\n", 10000)
	code := Run(ctx, args, strings.NewReader(input), &out, &errOut, BuildInfo{})
	if code != 0 || errOut.Len() != 0 {
		t.Fatal(code, errOut.String())
	}
	var accepted protocol.Accepted
	if err := json.Unmarshal(out.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code = Run(ctx, args, strings.NewReader(input), &out, &errOut, BuildInfo{})
	if code != 0 {
		t.Fatal(errOut.String())
	}
	var again protocol.Accepted
	json.Unmarshal(out.Bytes(), &again)
	if accepted != again {
		t.Fatal("retry duplicated task")
	}
	// Stop/resume are durable CLI mutations even when no model is available.
	for _, action := range []string{"stop", "resume"} {
		controlArgs := []string{"--profile", "cli-test", "--request-id", "cli-" + action, "agent", action, accepted.AgentID}
		out.Reset()
		errOut.Reset()
		if code := Run(ctx, controlArgs, nil, &out, &errOut, BuildInfo{}); code != 0 {
			t.Fatal(action, code, errOut.String())
		}
		first := out.String()
		out.Reset()
		if code := Run(ctx, controlArgs, nil, &out, &errOut, BuildInfo{}); code != 0 || out.String() != first {
			t.Fatal("control retry changed acceptance", action, out.String(), errOut.String())
		}
		snap, err := s.Snapshot(ctx, accepted.AgentID)
		if err != nil || snap.Agent.Paused != (action == "stop") {
			t.Fatal(snap, err)
		}
	}
	// A bad new configuration must not lock clients out of the running core.
	if err := os.MkdirAll(p.Config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Config, "config.yaml"), []byte("unknown: invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code = Run(ctx, []string{"--profile", "cli-test", "agent", "inspect", accepted.AgentID}, nil, &out, &errOut, BuildInfo{})
	if code != 0 {
		t.Fatal(errOut.String())
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil || len(snap.Tasks) != 1 || snap.Tasks[0].Input != input {
		t.Fatal(err, snap)
	}
	for _, plain := range []bool{false, true} {
		args := []string{"--profile", "cli-test", "tui", accepted.AgentID}
		if plain {
			args = append(args, "--plain")
		}
		out.Reset()
		errOut.Reset()
		code = Run(ctx, args, nil, &out, &errOut, BuildInfo{})
		var got protocol.Snapshot
		if code != 0 || json.Unmarshal(out.Bytes(), &got) != nil || got.Cursor != snap.Cursor || strings.Contains(out.String(), "\x1b") || errOut.Len() != 0 {
			t.Fatal("non-TTY UI must remain a read-only JSON query", code, out.String(), errOut.String())
		}
	}
	out.Reset()
	code = Run(ctx, []string{"--profile", "cli-test", "agent", "recovery", accepted.AgentID, "--task", accepted.TaskID}, nil, &out, &errOut, BuildInfo{})
	var recovery runtime.RecoveryReport
	if code != 0 || json.Unmarshal(out.Bytes(), &recovery) != nil || recovery.TaskID != accepted.TaskID || recovery.NextStep != "queued" || recovery.Cursor != snap.Cursor {
		t.Fatal(code, out.String(), errOut.String())
	}
	if _, err := s.ClaimNext(ctx, accepted.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	var firstRecovery string
	for range 2 {
		out.Reset()
		errOut.Reset()
		code = Run(ctx, []string{"--profile", "cli-test", "--request-id", "recover-once", "agent", "recover", accepted.AgentID, "--task", accepted.TaskID}, nil, &out, &errOut, BuildInfo{})
		if code != 0 || firstRecovery != "" && firstRecovery != out.String() {
			t.Fatal(code, out.String(), errOut.String())
		}
		firstRecovery = out.String()
	}
	missingCtx, end := context.WithTimeout(ctx, time.Second)
	defer end()
	out.Reset()
	errOut.Reset()
	code = Run(missingCtx, []string{"--profile", "offline", "core", "status"}, nil, &out, &errOut, BuildInfo{})
	if code == 0 || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatal("offline falsely succeeded")
	}
}
