package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
	"zende.local/harness/internal/protocol"
)

type Limits struct {
	Agents         int   `json:"agents" yaml:"agents"`
	Depth          int   `json:"depth" yaml:"depth"`
	Processes      int   `json:"processes" yaml:"processes"`
	OutputBytes    int64 `json:"output_bytes" yaml:"output_bytes"`
	ModelRetries   int   `json:"model_retries" yaml:"model_retries"`
	FormatRepairs  int   `json:"format_repairs" yaml:"format_repairs"`
	RetryBaseMS    int   `json:"retry_base_ms" yaml:"retry_base_ms"`
	ContextReserve int   `json:"context_reserve" yaml:"context_reserve"`
}

type ModelConfig struct {
	ID               string           `json:"id" yaml:"id"`
	Label            string           `json:"label,omitempty" yaml:"label,omitempty"`
	ContextWindow    int              `json:"context_window" yaml:"context_window"`
	MaxOutput        int              `json:"max_output" yaml:"max_output"`
	Efforts          []string         `json:"efforts,omitempty" yaml:"efforts,omitempty"`
	Images           bool             `json:"images,omitempty" yaml:"images,omitempty"`
	NativeCategories bool             `json:"native_categories,omitempty" yaml:"native_categories,omitempty"`
	ToolSearch       string           `json:"tool_search,omitempty" yaml:"tool_search,omitempty"`
	Chat             *ChatConfig      `json:"chat,omitempty" yaml:"chat,omitempty"`
	Anthropic        *AnthropicConfig `json:"anthropic,omitempty" yaml:"anthropic,omitempty"`
	Gemini           *GeminiConfig    `json:"gemini,omitempty" yaml:"gemini,omitempty"`
}

// ChatConfig declares wire differences for a specific configured model.
type ChatConfig struct {
	MaxTokensField  string `json:"max_tokens_field,omitempty" yaml:"max_tokens_field,omitempty"`
	InstructionRole string `json:"instruction_role,omitempty" yaml:"instruction_role,omitempty"`
	IncludeUsage    bool   `json:"include_usage,omitempty" yaml:"include_usage,omitempty"`
	ReasoningField  string `json:"reasoning_field,omitempty" yaml:"reasoning_field,omitempty"`
}

// AnthropicConfig describes only explicitly selected Messages API options.
type AnthropicConfig struct {
	Thinking            string `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	ThinkingBudget      int    `json:"thinking_budget_tokens,omitempty" yaml:"thinking_budget_tokens,omitempty"`
	ThinkingDisplay     string `json:"thinking_display,omitempty" yaml:"thinking_display,omitempty"`
	InterleavedThinking bool   `json:"interleaved_thinking,omitempty" yaml:"interleaved_thinking,omitempty"`
}

// GeminiConfig keeps an omitted budget distinct from zero (disabled) and -1
// (dynamic). Thinking levels are selected through the model's Efforts list.
type GeminiConfig struct {
	ThinkingBudget  *int `json:"thinking_budget_tokens,omitempty" yaml:"thinking_budget_tokens,omitempty"`
	IncludeThoughts bool `json:"include_thoughts,omitempty" yaml:"include_thoughts,omitempty"`
}

type Connection struct {
	Label      string        `json:"label,omitempty" yaml:"label,omitempty"`
	Vendor     string        `json:"vendor,omitempty" yaml:"vendor,omitempty"`
	Protocol   string        `json:"protocol" yaml:"protocol"`
	BaseURL    string        `json:"base_url" yaml:"base_url"`
	Credential string        `json:"credential,omitempty" yaml:"credential,omitempty"`
	Models     []ModelConfig `json:"models" yaml:"models"`
}

type Config struct {
	Model        string                `json:"model" yaml:"model"`
	Effort       string                `json:"effort" yaml:"effort"`
	Cwd          string                `json:"cwd" yaml:"cwd"`
	Shell        string                `json:"shell" yaml:"shell"`
	Limits       Limits                `json:"limits" yaml:"limits"`
	Connections  map[string]Connection `json:"connections" yaml:"connections"`
	ResourceDirs []string              `json:"resource_dirs" yaml:"resource_dirs"`
	Compaction   CompactionConfig      `json:"compaction" yaml:"compaction"`
}

type CompactionConfig struct {
	TriggerPercent   int `json:"trigger_percent" yaml:"trigger_percent"`
	SummaryTokens    int `json:"summary_tokens" yaml:"summary_tokens"`
	KeepRecentTokens int `json:"keep_recent_tokens" yaml:"keep_recent_tokens"`
	MaxAttempts      int `json:"max_attempts" yaml:"max_attempts"`
}

func (c Config) ContextBudgetPolicy() protocol.ContextBudgetPolicy {
	return protocol.ContextBudgetPolicy{Version: 1, TriggerPercent: c.Compaction.TriggerPercent,
		ReserveTokens: c.Limits.ContextReserve, SummaryTokens: c.Compaction.SummaryTokens,
		KeepRecentTokens: c.Compaction.KeepRecentTokens, MaxAttempts: c.Compaction.MaxAttempts}
}

type Effective struct {
	Config  Config            `json:"config"`
	Sources map[string]string `json:"sources"`
}

type Layer struct {
	Name string
	YAML []byte
}

func Defaults(cwd string) Config {
	return Config{Cwd: cwd, Shell: "/bin/sh", Connections: map[string]Connection{}, ResourceDirs: []string{},
		Compaction: CompactionConfig{TriggerPercent: 90, SummaryTokens: 2048, KeepRecentTokens: 8192, MaxAttempts: 2},
		Limits:     Limits{Agents: 8, Depth: 4, Processes: 16, OutputBytes: 64 << 20, ModelRetries: 2, FormatRepairs: 1, RetryBaseMS: 250, ContextReserve: 8192}}
}

// Compose replaces lists, recursively merges maps and rejects invalid input
// without mutating the caller's previous Effective snapshot.
func Compose(defaults Config, layers ...Layer) (Effective, error) {
	b, err := json.Marshal(defaults)
	if err != nil {
		return Effective{}, err
	}
	if len(b) > MaxConfigBytes {
		return Effective{}, protocol.E("resource_limit", "configuration exceeds total byte limit")
	}
	total := len(b)
	var merged map[string]any
	if err := yaml.Unmarshal(b, &merged); err != nil {
		return Effective{}, err
	}
	sources := map[string]string{}
	markSources(merged, "", "defaults", sources)
	for _, layer := range layers {
		total += len(layer.YAML)
		if len(layer.YAML) > MaxConfigLayerBytes || total > MaxConfigBytes {
			return Effective{}, protocol.E("resource_limit", "configuration exceeds layer or total byte limit")
		}
		if len(bytes.TrimSpace(layer.YAML)) == 0 {
			continue
		}
		var document yaml.Node
		d := yaml.NewDecoder(bytes.NewReader(layer.YAML))
		if err := d.Decode(&document); err != nil {
			return Effective{}, fmt.Errorf("%s: invalid YAML", layer.Name)
		}
		var extra yaml.Node
		if err := d.Decode(&extra); err != io.EOF {
			return Effective{}, fmt.Errorf("%s: expected one YAML document", layer.Name)
		}
		if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || !validConfigYAML(&document, 0) {
			return Effective{}, fmt.Errorf("%s: expected a bounded mapping without aliases, duplicate keys or null values", layer.Name)
		}
		var value map[string]any
		if document.Decode(&value) != nil {
			return Effective{}, fmt.Errorf("%s: invalid configuration values", layer.Name)
		}
		merge(merged, value, "", layer.Name, sources)
	}
	b, err = json.Marshal(merged)
	if err != nil {
		return Effective{}, fmt.Errorf("configuration values must be JSON-compatible")
	}
	if len(b) > MaxConfigBytes {
		return Effective{}, protocol.E("resource_limit", "configuration exceeds total byte limit")
	}
	var config Config
	if err := protocol.StrictDecode(b, &config); err != nil {
		return Effective{}, fmt.Errorf("configuration has unknown fields or invalid types")
	}
	if err := config.Validate(); err != nil {
		return Effective{}, err
	}
	effective := Effective{Config: config, Sources: sources}
	b, err = json.Marshal(effective)
	if err != nil || len(b) > MaxConfigBytes {
		return Effective{}, protocol.E("resource_limit", "configuration and sources exceed total byte limit")
	}
	return effective, nil
}

func validConfigYAML(n *yaml.Node, depth int) bool {
	if depth > 64 || n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!null" {
		return false
	}
	if n.Kind == yaml.MappingNode {
		if n.Tag != "!!map" || len(n.Content)%2 != 0 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
	}
	if n.Kind == yaml.SequenceNode && n.Tag != "!!seq" {
		return false
	}
	if n.Kind == yaml.ScalarNode && n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!float" && n.Tag != "!!bool" {
		return false
	}
	for _, child := range n.Content {
		if !validConfigYAML(child, depth+1) {
			return false
		}
	}
	return true
}

func merge(dst, src map[string]any, prefix, name string, sources map[string]string) {
	for key, value := range src {
		path := prefix + "/" + key
		child, isMap := value.(map[string]any)
		old, oldMap := dst[key].(map[string]any)
		if isMap && oldMap {
			merge(old, child, path, name, sources)
			continue
		}
		for p := range sources {
			if p == path || strings.HasPrefix(p, path+"/") {
				delete(sources, p)
			}
		}
		dst[key] = value
		if isMap {
			markSources(child, path, name, sources)
		} else {
			sources[path] = name
		}
	}
}

func markSources(m map[string]any, prefix, name string, out map[string]string) {
	for key, value := range m {
		path := prefix + "/" + key
		if child, ok := value.(map[string]any); ok {
			markSources(child, path, name, out)
		} else {
			out[path] = name
		}
	}
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validCredential(ref string) bool {
	if ref == "" {
		return true
	}
	if strings.HasPrefix(ref, "env:") {
		return envName.MatchString(strings.TrimPrefix(ref, "env:"))
	}
	if strings.HasPrefix(ref, "file:") {
		return filepath.IsAbs(strings.TrimPrefix(ref, "file:"))
	}
	return false
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.Cwd) || !filepath.IsAbs(c.Shell) {
		return fmt.Errorf("cwd and shell must be absolute paths")
	}
	if !c.ContextBudgetPolicy().Valid() {
		return fmt.Errorf("invalid context compaction settings")
	}
	l := c.Limits
	if l.Agents < 1 || l.Depth < 1 || l.Processes < 1 || l.OutputBytes < 1024 || l.ModelRetries < 0 || l.FormatRepairs < 0 || l.RetryBaseMS < 1 || l.ContextReserve < 1 {
		return fmt.Errorf("invalid runtime limits")
	}
	for _, dir := range c.ResourceDirs {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("resource directories must be absolute")
		}
	}
	for id, conn := range c.Connections {
		if !namePattern.MatchString(id) {
			return fmt.Errorf("invalid connection ID")
		}
		switch conn.Protocol {
		case "responses", "chat-completions", "anthropic", "gemini":
		default:
			return fmt.Errorf("unsupported model protocol")
		}
		u, err := url.Parse(conn.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("base_url must be an HTTP URL without credentials, query or fragment")
		}
		if !validCredential(conn.Credential) {
			return fmt.Errorf("credential must reference env:NAME or file:/absolute/path")
		}
		seen := map[string]bool{}
		for _, m := range conn.Models {
			if m.ID == "" || seen[m.ID] || m.ContextWindow < 1 || m.MaxOutput < 1 || m.MaxOutput >= m.ContextWindow {
				return fmt.Errorf("invalid or duplicate model definition")
			}
			seen[m.ID] = true
			if m.Chat != nil {
				o := m.Chat
				if conn.Protocol != "chat-completions" || o.MaxTokensField != "" && o.MaxTokensField != "max_tokens" && o.MaxTokensField != "max_completion_tokens" || o.InstructionRole != "" && o.InstructionRole != "system" && o.InstructionRole != "developer" || o.ReasoningField != "" && o.ReasoningField != "reasoning_content" && o.ReasoningField != "reasoning" && o.ReasoningField != "reasoning_text" {
					return fmt.Errorf("invalid Chat Completions compatibility settings")
				}
			}
			if o := m.Anthropic; o != nil {
				if conn.Protocol != "anthropic" {
					return fmt.Errorf("Anthropic settings require the anthropic protocol")
				}
				switch o.Thinking {
				case "", "disabled", "adaptive":
					if o.ThinkingBudget != 0 || o.InterleavedThinking {
						return fmt.Errorf("thinking budget and interleaving beta require enabled thinking")
					}
				case "enabled":
					if o.ThinkingBudget < 1024 || o.ThinkingBudget >= m.MaxOutput {
						return fmt.Errorf("thinking budget must be at least 1024 and smaller than max output")
					}
				default:
					return fmt.Errorf("unsupported Anthropic thinking mode")
				}
				if o.ThinkingDisplay != "" && (o.ThinkingDisplay != "summarized" && o.ThinkingDisplay != "omitted" || o.Thinking != "enabled" && o.Thinking != "adaptive") {
					return fmt.Errorf("thinking display requires enabled or adaptive thinking")
				}
			}
			if o := m.Gemini; o != nil {
				if conn.Protocol != "gemini" {
					return fmt.Errorf("Gemini settings require the gemini protocol")
				}
				if o.ThinkingBudget != nil && (*o.ThinkingBudget < -1 || *o.ThinkingBudget >= m.MaxOutput || len(m.Efforts) != 0) {
					return fmt.Errorf("Gemini thinking budget must be -1 or between zero and max output, and cannot be combined with effort options")
				}
			}
			if conn.Protocol == "gemini" {
				for _, effort := range m.Efforts {
					switch strings.ToUpper(effort) {
					case "MINIMAL", "LOW", "MEDIUM", "HIGH":
					default:
						return fmt.Errorf("unsupported Gemini thinking level")
					}
				}
			}
			if conn.Protocol != "responses" && m.NativeCategories {
				return fmt.Errorf("selected protocol uses explicit text markers, not native categories")
			}
			if m.ToolSearch != "" && m.ToolSearch != "host" && (m.ToolSearch != "responses-client" || conn.Protocol != "responses") {
				return fmt.Errorf("tool_search must be host or responses-client on a confirmed Responses endpoint/model")
			}
		}
	}
	if c.Model != "" {
		id, model, ok := strings.Cut(c.Model, "/")
		conn, exists := c.Connections[id]
		found := false
		for _, m := range conn.Models {
			if m.ID == model {
				found = true
				if c.Effort != "" {
					valid := false
					for _, e := range m.Efforts {
						valid = valid || e == c.Effort
					}
					if !valid {
						return fmt.Errorf("effort is not supported by the selected model")
					}
				}
			}
		}
		if !ok || !exists || !found {
			return fmt.Errorf("model must select a configured connection/model")
		}
	} else if c.Effort != "" {
		return fmt.Errorf("effort requires a selected model")
	}
	return nil
}

// ResolveCredential is used only at a provider connection boundary. Resolved
// values never become fields of Config, Effective or ordinary history.
func ResolveCredential(ref string, env func(string) string) (string, error) {
	if !validCredential(ref) {
		return "", fmt.Errorf("invalid credential reference")
	}
	if ref == "" {
		return "", nil
	}
	var value string
	if strings.HasPrefix(ref, "env:") {
		value = env(strings.TrimPrefix(ref, "env:"))
	} else {
		file, err := os.OpenFile(strings.TrimPrefix(ref, "file:"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", fmt.Errorf("credential file unavailable")
		}
		defer file.Close()
		if st, err := file.Stat(); err != nil || !st.Mode().IsRegular() {
			return "", fmt.Errorf("credential file must be regular")
		}
		b, err := io.ReadAll(io.LimitReader(file, 65537))
		if err != nil || len(b) > 65536 {
			return "", fmt.Errorf("credential file unreadable or too large")
		}
		value = strings.TrimSpace(string(b))
	}
	if value == "" {
		return "", fmt.Errorf("credential reference is unset")
	}
	return value, nil
}

// Load follows defaults < user < project < local < environment < CLI < task.
// It never resolves credentials or writes missing configuration files.
func Load(paths Paths, cwd string, env func(string) string, cli, task []byte) (Effective, error) {
	prepared, err := PrepareConfig(context.Background(), paths, cwd, env, cli, task)
	if err != nil {
		return Effective{}, err
	}
	return prepared.Snapshot().Effective, nil
}
