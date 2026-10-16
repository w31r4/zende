package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

// Exercise the documented example through the same actual CLI/UDS Core as the
// fixture suite. The plugin itself is the standalone main.go, not a test helper.
func checkExamplePluginCLI(t *testing.T, run func(bool, ...string) []byte) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "zende-plugin-echo")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../examples/plugin-echo/main.go").CombinedOutput(); err != nil {
		t.Fatalf("example build: %v\n%s", err, out)
	}
	raw, err := os.ReadFile("../../examples/plugin-echo/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest plugin.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		t.Fatal("invalid example manifest")
	}
	manifest.Command = binary
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var agent, installed protocol.Accepted
	if err := json.Unmarshal(run(true, "agent", "create", "--cwd", dir), &agent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run(true, "--request-id", "example-install", "resource", "register", agent.AgentID, source, "--kind", "plugin"), &installed); err != nil || installed.Resource == nil {
		t.Fatal(installed, err)
	}
	var catalog resource.PluginCatalog
	if err := json.Unmarshal(run(true, "plugin", "catalog", agent.AgentID, installed.Resource.ID), &catalog); err != nil {
		t.Fatal(err)
	}
	var ref string
	for _, d := range catalog.Definitions {
		if d.Kind == "command" && d.Name == "echo" {
			ref = d.Ref
		}
	}
	if ref == "" {
		t.Fatal("example has no command")
	}
	arguments := filepath.Join(dir, "arguments.json")
	if err := os.WriteFile(arguments, []byte(`{"text":"hello 中文","delay_ms":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	call := []string{"--request-id", "example-call", "plugin", "command", agent.AgentID, installed.Resource.ID, ref, "--file", arguments}
	run(true, call...)
	var receipt store.PluginReceipt
	for deadline := time.Now().Add(5 * time.Second); ; {
		if err := json.Unmarshal(run(true, "plugin", "inspect", agent.AgentID, "example-call"), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.State == "completed" {
			break
		}
		if receipt.State != "dispatched" || time.Now().After(deadline) {
			t.Fatal("example did not complete", receipt)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var progress store.PluginProgressPage
	if err := json.Unmarshal(run(true, "plugin", "progress", agent.AgentID, "example-call"), &progress); err != nil || len(progress.Items) != 1 {
		t.Fatal(progress, err)
	}
	var page struct {
		Text string `json:"text"`
		More bool   `json:"more"`
	}
	if err := json.Unmarshal(run(true, "plugin", "result", agent.AgentID, "example-call"), &page); err != nil || page.More {
		t.Fatal(page, err)
	}
	var result plugin.CallResult
	if json.Unmarshal([]byte(page.Text), &result) != nil || result.Completion == nil || result.State != "completed" {
		t.Fatal(page)
	}
	var output map[string]string
	if json.Unmarshal(result.Completion.Output, &output) != nil || output["text"] != "hello 中文" {
		t.Fatal(output)
	}
	var retry store.PluginReceipt
	if err := json.Unmarshal(run(true, call...), &retry); err != nil || retry.Binding != receipt.Binding || retry.State != "completed" {
		t.Fatal(retry, err)
	}
}
