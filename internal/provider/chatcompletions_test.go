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
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func TestChatBindingCatalogAndOfflineRestore(t *testing.T) {
	var requests, resolved atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer FIXTURE_ROTATED" {
			t.Error("transport did not restore original binding")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "original" || body["max_tokens"] != float64(1024) || body["max_completion_tokens"] != nil || body["stream_options"] != nil || body["reasoning_effort"] != "high" {
			t.Error("compatibility options changed", body)
		}
		if body["messages"].([]any)[0].(map[string]any)["role"] != "developer" {
			t.Error(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<final>\\ndone\\n</final>\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	options := &resource.ChatConfig{MaxTokensField: "max_tokens", InstructionRole: "developer", ReasoningField: "reasoning_content"}
	cfg := resource.Defaults(t.TempDir())
	cfg.Model, cfg.Effort = "local/original", "high"
	cfg.Connections["local"] = resource.Connection{Protocol: "chat-completions", BaseURL: server.URL, Credential: "env:ORIGINAL_KEY", Models: []resource.ModelConfig{{ID: "original", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"high"}, Images: true, Chat: options}}}
	original, err := New(cfg, func(string) string { t.Fatal("resolved credential before sending"); return "" })
	if err != nil {
		t.Fatal(err)
	}
	options.MaxTokensField = "changed-after-snapshot"
	page, err := original.Connections(context.Background(), protocol.ConnectionQuery{})
	if err != nil || len(page.Items) != 1 || !page.Items[0].Supported || requests.Load() != 0 {
		t.Fatal(page, err)
	}
	binding, err := original.Resolve(context.Background(), cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(resource.Defaults(t.TempDir()), func(key string) string {
		resolved.Add(1)
		if key != "ORIGINAL_KEY" {
			t.Error(key)
		}
		return "FIXTURE_ROTATED"
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Restore(context.Background(), binding.Snapshot)
	if err != nil || restored.Protocol != "chat-completions" || restored.Model != "original" || restored.NativeCategories || !restored.Images || resolved.Load() != 0 {
		t.Fatal(restored, err)
	}
	var collector model.Collector
	if err := restored.Adapter.Stream(context.Background(), model.Request{Model: restored.Model, System: "instructions", MaxOutputTokens: restored.MaxOutputTokens}, collector.Add); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Response(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || resolved.Load() != 1 || strings.Contains(string(restored.Snapshot), "FIXTURE_ROTATED") {
		t.Fatal("credential or request lifecycle changed")
	}
	for _, mutate := range []func(*bindingSnapshot){
		func(s *bindingSnapshot) { s.Connection.Models[0].Chat.MaxTokensField = "invalid" },
		func(s *bindingSnapshot) { s.Connection.Models[0].Chat.InstructionRole = "user" },
		func(s *bindingSnapshot) { s.Connection.Models[0].Chat.ReasoningField = "content" },
		func(s *bindingSnapshot) { s.Connection.Models[0].NativeCategories = true },
		func(s *bindingSnapshot) { s.Connection.Protocol = "responses" },
	} {
		var saved bindingSnapshot
		json.Unmarshal(binding.Snapshot, &saved)
		mutate(&saved)
		raw, _ := json.Marshal(saved)
		if _, err := restarted.Restore(context.Background(), raw); err == nil {
			t.Fatal("invalid saved compatibility accepted")
		}
	}
}
