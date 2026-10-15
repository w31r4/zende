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

func TestConfiguredBindingAndLateCredentialResolution(t *testing.T) {
	var resolved atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TEST_CHANGED" {
			t.Error("stale credential")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "family/model" || body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Error(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	cfg := resource.Defaults(t.TempDir())
	cfg.Model = "private/family/model"
	cfg.Effort = "high"
	cfg.Connections["private"] = resource.Connection{Protocol: "responses", BaseURL: server.URL, Credential: "env:TEST_KEY", Models: []resource.ModelConfig{{ID: "family/model", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"high"}, NativeCategories: true}}}
	key := "TEST_OLD"
	r, err := New(cfg, func(name string) string {
		resolved.Add(1)
		if name != "TEST_KEY" {
			t.Error(name)
		}
		return key
	})
	if err != nil {
		t.Fatal(err)
	}
	// Changes to caller-owned config cannot alter an existing resolver snapshot.
	delete(cfg.Connections, "private")
	b, err := r.Resolve(context.Background(), "private/family/model")
	if err != nil || b.Model != "family/model" || b.Protocol != "responses" || !b.NativeCategories || b.MaxOutputTokens != 1024 || resolved.Load() != 0 {
		t.Fatal(b, err)
	}
	key = "TEST_CHANGED"
	err = b.Adapter.Stream(context.Background(), model.Request{Model: b.Model, MaxOutputTokens: b.MaxOutputTokens}, func(model.Delta) error { return nil })
	if err != nil || resolved.Load() != 1 {
		t.Fatal(err, resolved.Load())
	}
	data, _ := json.Marshal(r.config)
	if strings.Contains(string(data), key) {
		t.Fatal("credential persisted")
	}
	for _, selection := range []string{"", "unknown/model", "private/unknown"} {
		if _, err := r.Resolve(context.Background(), selection); err == nil {
			t.Fatal(selection)
		}
	}
}
