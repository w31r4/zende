package provider

import (
	"context"
	"encoding/json"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func TestExplicitSelectionEffortAndCatalogFence(t *testing.T) {
	ctx := context.Background()
	cfg := resource.Defaults(t.TempDir())
	cfg.Model, cfg.Effort = "local/family/model", "high"
	cfg.Connections["local"] = resource.Connection{Protocol: "responses", BaseURL: "https://test.invalid/v1", Credential: "env:UNRESOLVED_TEST_KEY", Models: []resource.ModelConfig{{ID: "family/model", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"low", "high"}}}}
	r, err := New(cfg, func(string) string { t.Error("selection resolved credentials"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.Connections(ctx, protocol.ConnectionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"", "low", "high"} {
		b, err := r.ResolveSelection(ctx, cfg.Model, effort, page.Version)
		if err != nil {
			t.Fatal(err)
		}
		var saved bindingSnapshot
		if err := json.Unmarshal(b.Snapshot, &saved); err != nil || saved.Effort != effort {
			t.Fatal("profile default overrode explicit effort", saved, err)
		}
		// Recovery does not depend on the current catalog/default.
		empty, err := New(resource.Defaults("/"), func(string) string { t.Error("restore resolved credentials"); return "" })
		if err != nil {
			t.Fatal(err)
		}
		restored, err := empty.Restore(ctx, b.Snapshot)
		if err != nil || string(restored.Snapshot) != string(b.Snapshot) || restored.Model != b.Model {
			t.Fatal(restored, err)
		}
	}
	defaultBinding, err := r.Resolve(ctx, cfg.Model)
	var saved bindingSnapshot
	if err != nil || json.Unmarshal(defaultBinding.Snapshot, &saved) != nil || saved.Effort != "high" {
		t.Fatal(saved, err)
	}
	for _, p := range [][3]string{{cfg.Model, "ultra", page.Version}, {cfg.Model, "low", "stale"}, {"local/absent", "", ""}} {
		if _, err := r.ResolveSelection(ctx, p[0], p[1], p[2]); err == nil {
			t.Fatal("accepted invalid selection", p)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.ResolveSelection(cancelled, cfg.Model, "", ""); err == nil {
		t.Fatal("ignored cancellation")
	}
}
