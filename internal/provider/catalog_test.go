package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func TestCatalogConfiguredConnectionsVersionsAndNoCredentials(t *testing.T) {
	ctx := context.Background()
	cfg := resource.Defaults(t.TempDir())
	newResolver := func() *Resolver {
		t.Helper()
		r, err := New(cfg, func(string) string { t.Fatal("catalog resolved a credential"); return "" })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	empty := newResolver()
	page, err := empty.Connections(ctx, protocol.ConnectionQuery{})
	if err != nil || len(page.Items) != 0 || page.Items == nil || page.DefaultModel != "" || page.Next != "" {
		t.Fatal(page, err)
	}
	for _, id := range []string{"two", "one", "unsupported"} {
		cfg.Connections[id] = resource.Connection{Label: "自定义连接", Vendor: "kimi", Protocol: "responses", BaseURL: "https://" + id + ".invalid/private", Credential: "file:/MISSING_PRIVATE_KEY", Models: []resource.ModelConfig{
			{ID: "org/z", ContextWindow: 8192, MaxOutput: 1024},
			{ID: "org/a", Label: "小模型", ContextWindow: 4096, MaxOutput: 512, Efforts: []string{"low", "high"}, Images: true, NativeCategories: true},
		}}
	}
	c := cfg.Connections["unsupported"]
	c.Protocol = "anthropic"
	cfg.Connections["unsupported"] = c
	cfg.Model, cfg.Effort = "one/org/a", "high"
	r := newResolver()
	first, err := r.Connections(ctx, protocol.ConnectionQuery{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "one" || !first.Items[0].Supported || first.Next != "one" || first.Version == page.Version || first.DefaultModel != cfg.Model || first.Effort != "high" {
		t.Fatal(first, err)
	}
	second, err := r.Connections(ctx, protocol.ConnectionQuery{After: first.Next, Version: first.Version})
	if err != nil || len(second.Items) != 2 || second.Items[0].ID != "two" || second.Items[1].Supported || second.Next != "" {
		t.Fatal(second, err)
	}
	models, err := r.ListModels(ctx, protocol.ModelQuery{Connection: "one", Limit: 1, Version: first.Version})
	if err != nil || len(models.Items) != 1 || models.Items[0].ID != "org/a" || !models.Items[0].Images || models.Next != "org/a" || !reflect.DeepEqual(models.Items[0].Efforts, []string{"low", "high"}) {
		t.Fatal(models, err)
	}
	last, err := r.ListModels(ctx, protocol.ModelQuery{Connection: "one", Limit: 1, After: models.Next})
	if err != nil || len(last.Items) != 1 || last.Items[0].ID != "org/z" || last.Items[0].Efforts == nil || last.Next != "" {
		t.Fatal(last, err)
	}
	for _, value := range []any{first, second, models, last} {
		data, _ := json.Marshal(value)
		for _, hidden := range []string{"MISSING_PRIVATE_KEY", "https://", "credential", "base_url"} {
			if strings.Contains(string(data), hidden) {
				t.Fatal("connection details in metadata catalog", string(data))
			}
		}
	}
	models.Items[0].Efforts[0] = "tampered"
	first.Items[0].Label = "tampered"
	c = cfg.Connections["one"]
	c.Models[1].Efforts[0] = "caller-change"
	cfg.Connections["one"] = c
	again, err := r.ListModels(ctx, protocol.ModelQuery{Connection: "one"})
	if err != nil || again.Items[0].Efforts[0] != "low" || again.Connection.Label != "自定义连接" {
		t.Fatal("catalog shares mutable data", again, err)
	}
	delete(cfg.Connections, "two")
	changed := newResolver()
	if _, err := changed.ListModels(ctx, protocol.ModelQuery{Connection: "one", Version: r.version}); err == nil {
		t.Fatal("stale multi-step selection accepted")
	}
}

func TestCatalogQueryValidationCancellationAndFrameLimit(t *testing.T) {
	cfg := resource.Defaults(t.TempDir())
	cfg.Connections["local"] = resource.Connection{Protocol: "responses", BaseURL: "https://example.invalid", Models: []resource.ModelConfig{}}
	r, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []protocol.ConnectionQuery{{Limit: -1}, {Limit: 101}, {After: "\x00"}, {After: string([]byte{0xff})}, {Version: "old"}} {
		if _, err := r.Connections(ctx, q); err == nil {
			t.Fatal(q)
		}
	}
	if _, err := r.ListModels(ctx, protocol.ModelQuery{Connection: "unknown"}); err == nil {
		t.Fatal("unknown connection accepted")
	}
	models, err := r.ListModels(ctx, protocol.ModelQuery{Connection: "local"})
	if err != nil || models.Items == nil || len(models.Items) != 0 {
		t.Fatal(models, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Connections(cancelled, protocol.ConnectionQuery{}); err == nil {
		t.Fatal("cancel ignored")
	}
	c := cfg.Connections["local"]
	c.Label = strings.Repeat("x", 1<<20)
	cfg.Connections["local"] = c
	r, err = New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Connections(ctx, protocol.ConnectionQuery{}); err == nil {
		t.Fatal("oversized IPC catalog")
	}
}
