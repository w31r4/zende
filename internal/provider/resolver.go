// Package provider binds configured connections to model transports. The loop
// sees only model.Binding; it never imports endpoint or credential handling.
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/anthropic"
	"zende.local/harness/internal/model/chatcompletions"
	"zende.local/harness/internal/model/gemini"
	"zende.local/harness/internal/model/responses"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

type Resolver struct {
	config  resource.Config
	env     func(string) string
	version string
}

type bindingSnapshot struct {
	Version    int                 `json:"version"`
	Selection  string              `json:"selection"`
	Connection resource.Connection `json:"connection"`
	Effort     string              `json:"effort"`
}

func New(config resource.Config, env func(string) string) (*Resolver, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var snapshot resource.Config
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	if env == nil {
		env = os.Getenv
	}
	digest := sha256.Sum256(data)
	return &Resolver{config: snapshot, env: env, version: hex.EncodeToString(digest[:])}, nil
}

func (r *Resolver) Resolve(ctx context.Context, selection string) (model.Binding, error) {
	effort := ""
	if selection == r.config.Model {
		effort = r.config.Effort
	}
	return r.ResolveSelection(ctx, selection, effort, "")
}

func (r *Resolver) ResolveSelection(ctx context.Context, selection, effort, version string) (model.Binding, error) {
	if err := ctx.Err(); err != nil {
		return model.Binding{}, err
	}
	if version != "" && version != r.version {
		return model.Binding{}, protocol.E("catalog_changed", "model configuration changed; restart selection")
	}
	connection, id, ok := strings.Cut(selection, "/")
	conn, exists := r.config.Connections[connection]
	if !ok || !exists {
		return model.Binding{}, protocol.E("model_unavailable", "select a configured connection/model")
	}
	var selected *resource.ModelConfig
	for _, m := range conn.Models {
		if m.ID == id {
			selected = &m
			break
		}
	}
	if selected == nil {
		return model.Binding{}, protocol.E("model_unavailable", "selected model is not configured")
	}
	if effort != "" {
		found := false
		for _, option := range selected.Efforts {
			found = found || option == effort
		}
		if !found {
			return model.Binding{}, protocol.E("unsupported_capability", "selected model does not support this effort")
		}
	}
	// Retain only the selected model, not every connection in the catalog.
	conn.Models = []resource.ModelConfig{*selected}
	return r.bind(ctx, bindingSnapshot{Version: 1, Selection: selection, Connection: conn, Effort: effort})
}

func (r *Resolver) Restore(ctx context.Context, raw json.RawMessage) (model.Binding, error) {
	var saved bindingSnapshot
	if len(raw) > model.MaxBlockBytes || protocol.StrictDecode(raw, &saved) != nil || saved.Version != 1 || len(saved.Connection.Models) != 1 {
		return model.Binding{}, protocol.E("model_snapshot_invalid", "saved model binding is missing, invalid or unsupported")
	}
	connection, _, ok := strings.Cut(saved.Selection, "/")
	check := resource.Defaults("/")
	check.Model, check.Effort = saved.Selection, saved.Effort
	check.Connections[connection] = saved.Connection
	if !ok || check.Validate() != nil {
		return model.Binding{}, protocol.E("model_snapshot_invalid", "saved model settings failed validation")
	}
	return r.bind(ctx, saved)
}

func (r *Resolver) bind(ctx context.Context, saved bindingSnapshot) (model.Binding, error) {
	if err := ctx.Err(); err != nil {
		return model.Binding{}, err
	}
	conn, selected := saved.Connection, saved.Connection.Models[0]
	credential := func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return resource.ResolveCredential(conn.Credential, r.env)
	}
	var adapter model.Adapter
	switch conn.Protocol {
	case gemini.Protocol:
		config := gemini.Config{Endpoint: conn.BaseURL, Effort: saved.Effort, Credential: credential, Images: selected.Images}
		if options := selected.Gemini; options != nil {
			config.ThinkingBudget, config.IncludeThoughts = options.ThinkingBudget, options.IncludeThoughts
		}
		value, err := gemini.New(config)
		if err != nil {
			return model.Binding{}, err
		}
		adapter = value
	case anthropic.Protocol:
		config := anthropic.Config{Endpoint: conn.BaseURL, Effort: saved.Effort, Credential: credential, Images: selected.Images}
		if options := selected.Anthropic; options != nil {
			config.Thinking, config.ThinkingBudget = options.Thinking, options.ThinkingBudget
			config.ThinkingDisplay, config.InterleavedThinking = options.ThinkingDisplay, options.InterleavedThinking
		}
		value, err := anthropic.New(config)
		if err != nil {
			return model.Binding{}, err
		}
		adapter = value
	case chatcompletions.Protocol:
		config := chatcompletions.Config{Endpoint: conn.BaseURL, Effort: saved.Effort, Credential: credential, Images: selected.Images}
		if options := selected.Chat; options != nil {
			config.MaxTokensField, config.InstructionRole, config.IncludeUsage, config.ReasoningField = options.MaxTokensField, options.InstructionRole, options.IncludeUsage, options.ReasoningField
		}
		value, err := chatcompletions.New(config)
		if err != nil {
			return model.Binding{}, err
		}
		adapter = value
	case responses.Protocol:
		value, err := responses.New(responses.Config{Endpoint: conn.BaseURL, Effort: saved.Effort, Credential: credential, Images: selected.Images, ClientToolSearch: selected.ToolSearch == "responses-client"})
		if err != nil {
			return model.Binding{}, err
		}
		adapter = value
	default:
		return model.Binding{}, protocol.E("unsupported_capability", "configured model protocol adapter is not implemented yet")
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return model.Binding{}, err
	}
	search := selected.ToolSearch
	if search == "host" {
		search = ""
	}
	return model.Binding{Adapter: adapter, Model: selected.ID, Protocol: conn.Protocol, NativeCategories: selected.NativeCategories, ToolSearch: search, Images: selected.Images, MaxOutputTokens: selected.MaxOutput, ContextWindow: selected.ContextWindow, Snapshot: data}, nil
}

var _ model.Restorer = (*Resolver)(nil)
