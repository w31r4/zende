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
)

func TestProcessCLIControlAndBinaryInput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	a := &app{profile: "controls"}
	p, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureState(); err != nil {
		t.Fatal(err)
	}
	m, err := process.NewManager(filepath.Join(p.State, "processes"), "/bin/sh", 4, 65536)
	if err != nil {
		t.Fatal(err)
	}
	server, err := protocol.Listen(p.RunnerSocket, process.Service{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	run := func(input []byte, args ...string) (int, []byte, string) {
		var out, diagnostic bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "controls"}, args...), bytes.NewReader(input), &out, &diagnostic, BuildInfo{})
		return code, out.Bytes(), diagnostic.String()
	}
	code, out, diagnostic := run(nil, "--request-id", "cli-pipe", "process", "exec", "cat")
	if code != 0 || diagnostic != "" {
		t.Fatal(code, string(out), diagnostic)
	}
	defer func() {
		_, _ = m.Signal(ctx, "cleanup", process.SignalParams{ID: "cli-pipe", Signal: "KILL"})
		_, _ = m.Wait(ctx, "cli-pipe", time.Second)
	}()
	code, out, diagnostic = run(nil, "process", "list", "--limit", "1")
	var page process.ListPage
	if code != 0 || json.Unmarshal(out, &page) != nil || len(page.Processes) != 1 || page.Processes[0].ID != "cli-pipe" || page.Next != "" {
		t.Fatal("process list", string(out), diagnostic)
	}
	data := []byte{0xff, 0x00, 'x', '\n'}
	for range 2 {
		code, out, diagnostic = run(data, "--request-id", "cli-input", "process", "stdin", "cli-pipe", "--file", "-", "--close")
		if code != 0 || diagnostic != "" {
			t.Fatal(code, string(out), diagnostic)
		}
		var result process.ControlResult
		if err := json.Unmarshal(out, &result); err != nil || result.Written != len(data) || result.OperationID != "cli-input" {
			t.Fatal(string(out), err)
		}
	}
	code, out, diagnostic = run(nil, "process", "wait", "cli-pipe", "--wait-ms", "3000")
	if code != 0 {
		t.Fatal(string(out), diagnostic)
	}
	code, out, diagnostic = run(nil, "process", "output", "cli-pipe")
	var output process.Output
	if code != 0 || json.Unmarshal(out, &output) != nil || !bytes.Equal(output.Data, data) || output.Text != "\\xff\\x00x\n" {
		t.Fatal(string(out), diagnostic)
	}
	code, out, diagnostic = run(nil, "process", "control", "cli-pipe", "cli-input")
	var record process.ControlRecord
	if code != 0 || json.Unmarshal(out, &record) != nil || record.Status != "completed" {
		t.Fatal(string(out), diagnostic)
	}
	for _, args := range [][]string{
		{"process", "stdin", "cli-pipe", "ambiguous", "--file", "-"},
		{"process", "stdin", "cli-pipe"},
		{"process", "resize", "cli-pipe", "--cols", "80", "--rows", "24"},
		{"process", "attach", "cli-pipe"},
		{"--json", "process", "attach", "cli-pipe"},
	} {
		code, out, diagnostic = run(nil, args...)
		if code == 0 || len(out) != 0 || diagnostic == "" {
			t.Fatal("error corrupted stdout", args, code, string(out), diagnostic)
		}
	}
	for _, name := range []string{"list", "attach", "stdin", "resize", "cancel", "terminate", "control", "retention", "pin", "unpin", "rotate", "rotation"} {
		code, out, diagnostic = run(nil, "process", name, "--help")
		if code != 0 || diagnostic != "" || !strings.Contains(string(out), name) {
			t.Fatal("missing help", name, string(out), diagnostic)
		}
	}
	for range 2 {
		code, out, diagnostic = run(nil, "--request-id", "keep-output", "process", "pin", "cli-pipe", "--reference", "cli-history")
		if code != 0 {
			t.Fatal(string(out), diagnostic)
		}
	}
	code, out, diagnostic = run(nil, "process", "retention")
	var usage process.RetentionStatus
	if code != 0 || json.Unmarshal(out, &usage) != nil || usage.ProtectedBytes != int64(len(data)) || usage.HeldReferences != 1 {
		t.Fatal(string(out), diagnostic)
	}
	for _, args := range [][]string{
		{"process", "rotate", "--retain-bytes", "0"},
		{"process", "rotate", "--yes"},
		{"process", "rotate", "--retain-bytes", "0", "--yes"}, // Still referenced.
		{"process", "pin", "cli-pipe"},
	} {
		code, out, diagnostic = run(nil, args...)
		if code == 0 || len(out) != 0 || diagnostic == "" {
			t.Fatal(args, code, string(out), diagnostic)
		}
	}
	code, out, diagnostic = run(nil, "--request-id", "release-output", "process", "unpin", "cli-pipe", "--reference", "cli-history")
	if code != 0 {
		t.Fatal(string(out), diagnostic)
	}
	code, out, diagnostic = run(nil, "process", "rotate", "--retain-bytes", "0", "--dry-run")
	var rotation process.Rotation
	if code != 0 || json.Unmarshal(out, &rotation) != nil || rotation.Status != "planned" || len(rotation.Targets) != 1 {
		t.Fatal(string(out), diagnostic)
	}
	if actual, err := m.Read(ctx, "cli-pipe", "stdout", 0, 100); err != nil || !bytes.Equal(actual.Data, data) {
		t.Fatal("dry-run deleted output", actual, err)
	}
	for range 2 {
		code, out, diagnostic = run(nil, "--request-id", "rotate-once", "process", "rotate", "--retain-bytes", "0", "--yes")
		if code != 0 || json.Unmarshal(out, &rotation) != nil || rotation.FreedBytes != int64(len(data)) || rotation.Status != "completed" {
			t.Fatal(string(out), diagnostic)
		}
	}
	code, out, diagnostic = run(nil, "process", "rotation", "rotate-once")
	if code != 0 || json.Unmarshal(out, &rotation) != nil || rotation.OperationID != "rotate-once" {
		t.Fatal(string(out), diagnostic)
	}
}
