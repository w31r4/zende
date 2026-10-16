package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"zende.local/harness/internal/resource/plugin"
)

func TestExamplePluginThroughHost(t *testing.T) {
	// Build only main.go: the runnable example has no dependency on Zende's
	// internal packages, test fixture, module or SDK.
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende-plugin-echo")
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), source, 0600); err != nil {
		t.Fatal(err)
	}
	build, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(build, "go", "build", "-o", binary, "main.go")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOWORK=off", "GO111MODULE=off")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build standalone example: %v\n%s", err, out)
	}
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest plugin.Manifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.Validate() != nil {
		t.Fatal("invalid example manifest")
	}
	manifest.Command = binary
	i, err := plugin.Start(t.Context(), filepath.Join(dir, "manifest.json"), manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	catalog, err := i.Discover(t.Context())
	if err != nil || len(catalog.Definitions()) != 2 {
		t.Fatal(catalog, err)
	}
	if !i.Info().Trusted || i.Info().Limits.ConcurrentRequests != 8 {
		t.Fatal("missing trust/limit information", i.Info())
	}
	prepare := func(kind, operation string, delay int) *plugin.Call {
		t.Helper()
		for _, d := range catalog.Definitions() {
			if d.Kind != kind {
				continue
			}
			args, _ := json.Marshal(map[string]any{"text": "hello 中文 <&>", "delay_ms": delay})
			call, err := i.PrepareCall(d.Ref, operation, plugin.Scope{AgentID: "agent", Cwd: dir}, args)
			if err != nil {
				t.Fatal(err)
			}
			return call
		}
		t.Fatal("missing definition", kind)
		return nil
	}
	for _, kind := range []string{"tool", "command"} {
		call := prepare(kind, kind, 0)
		var progress []plugin.Progress
		result, err := call.Invoke(t.Context(), func(p plugin.Progress) error { progress = append(progress, p); return nil })
		if err != nil || result.State != "completed" || result.Completion == nil || len(progress) != 1 || progress[0].OperationID != kind || progress[0].RequestID != result.RequestID || progress[0].Sequence != 1 {
			t.Fatal(result, progress, err)
		}
		var output map[string]string
		if json.Unmarshal(result.Completion.Output, &output) != nil || output["text"] != "hello 中文 <&>" {
			t.Fatal(output)
		}
	}
	// A pending command must not block another call or its own cancellation.
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	started := make(chan struct{})
	type outcome struct {
		result plugin.CallResult
		err    error
	}
	done := make(chan outcome, 1)
	waiting := prepare("command", "waiting", 60000)
	go func() {
		r, err := waiting.Invoke(ctx, func(plugin.Progress) error { close(started); return nil })
		done <- outcome{r, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("example did not report progress")
	}
	fast, err := prepare("tool", "alongside", 0).Invoke(t.Context(), func(plugin.Progress) error { return nil })
	if err != nil || fast.State != "completed" {
		t.Fatal("waiting call blocked independent work", fast, err)
	}
	stop()
	select {
	case got := <-done:
		if got.err != nil || got.result.State != "cancelled" || got.result.OperationID != "waiting" || got.result.Completion == nil {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("example did not confirm cancellation")
	}
	if i.Info().State != "ready" {
		t.Fatal("cancel poisoned instance", i.Info())
	}
	if err := i.Close(); err != nil || i.Info().State != "closed" {
		t.Fatal("shutdown", err, i.Info())
	}
}
