package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/resource"
)

func TestBindingRestoresOriginalEndpointModelAndCredentialReference(t *testing.T) {
	var requests, resolved atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-rotated" {
			t.Error("did not resolve the original credential reference")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "old-model" || body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Error(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	cfg := resource.Defaults(t.TempDir())
	cfg.Model = "old/old-model"
	cfg.Effort = "high"
	cfg.Connections["old"] = resource.Connection{Protocol: "responses", BaseURL: server.URL, Credential: "env:ORIGINAL_KEY", Models: []resource.ModelConfig{{ID: "old-model", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"high"}, NativeCategories: true, Images: true}, {ID: "unselected", ContextWindow: 16384, MaxOutput: 2048}}}
	original, err := New(cfg, func(string) string { t.Fatal("credential resolved while snapshotting"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	b, err := original.Resolve(context.Background(), cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b.Snapshot), "unselected") {
		t.Fatal("snapshot captured the full catalog")
	}
	// A restarted core can restore even after the old connection was removed.
	restarted, err := New(resource.Defaults(t.TempDir()), func(name string) string {
		resolved.Add(1)
		if name != "ORIGINAL_KEY" {
			t.Error(name)
		}
		return "fixture-rotated"
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Restore(context.Background(), b.Snapshot)
	if err != nil || restored.Model != b.Model || restored.Protocol != b.Protocol || restored.NativeCategories != b.NativeCategories || restored.MaxOutputTokens != b.MaxOutputTokens || !restored.Images || restored.Images != b.Images || resolved.Load() != 0 {
		t.Fatal(restored, err)
	}
	if err := restored.Adapter.Stream(context.Background(), model.Request{Model: restored.Model, MaxOutputTokens: restored.MaxOutputTokens}, func(model.Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if resolved.Load() != 1 || requests.Load() != 1 || strings.Contains(string(restored.Snapshot), "fixture-rotated") {
		t.Fatal("restoration or credential persistence failed")
	}
	for _, mutate := range []func(*bindingSnapshot){
		func(s *bindingSnapshot) { s.Version = 2 },
		func(s *bindingSnapshot) { s.Connection.Credential = "literal-secret" },
		func(s *bindingSnapshot) { s.Connection.BaseURL = "https://example.invalid/?key=literal-secret" },
		func(s *bindingSnapshot) { s.Selection = "different/model" },
		func(s *bindingSnapshot) { s.Connection.Models = nil },
	} {
		var saved bindingSnapshot
		if json.Unmarshal(b.Snapshot, &saved) != nil {
			t.Fatal("invalid fixture")
		}
		mutate(&saved)
		raw, _ := json.Marshal(saved)
		if _, err := restarted.Restore(context.Background(), raw); err == nil || strings.Contains(err.Error(), "literal-secret") {
			t.Fatal(err)
		}
	}
}
