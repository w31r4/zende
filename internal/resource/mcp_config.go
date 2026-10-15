package resource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"zende.local/harness/internal/protocol"
)

const MaxMCPConfigBytes = 128 << 10

// MCPConfig contains only launch settings and credential references. Arguments
// and URLs are nonsecret configuration; secrets belong in env/header references.
type MCPConfig struct {
	SchemaVersion int               `json:"schema_version" yaml:"schema_version"`
	Name          string            `json:"name" yaml:"name"`
	Description   string            `json:"description,omitempty" yaml:"description,omitempty"`
	Transport     string            `json:"transport" yaml:"transport"`
	Command       string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args          []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Cwd           string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL           string            `json:"url,omitempty" yaml:"url,omitempty"`
	Credential    string            `json:"credential,omitempty" yaml:"credential,omitempty"`
	Headers       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

var mcpEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var mcpHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func ParseMCPConfig(data []byte) (MCPConfig, error) {
	var c MCPConfig
	fail := func() (MCPConfig, error) {
		return MCPConfig{}, protocol.E("invalid_resource", "invalid MCP configuration; use schema_version 1 and credential references")
	}
	if len(data) > MaxMCPConfigBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fail()
	}
	var node yaml.Node
	if yaml.Unmarshal(data, &node) != nil || len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return fail()
	}
	var valid func(*yaml.Node, int) bool
	valid = func(n *yaml.Node, depth int) bool {
		if depth > 8 || n.Kind == yaml.AliasNode || n.Tag == "!!null" {
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return false
				}
				seen[key.Value] = true
			}
		}
		for _, child := range n.Content {
			if !valid(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !valid(&node, 0) {
		return fail()
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF {
		return fail()
	}
	if c.SchemaVersion != 1 || !skillName.MatchString(c.Name) || len(c.Name) > 64 || len(c.Description) > 4096 || strings.ContainsAny(c.Description, "\x00\r") {
		return fail()
	}
	validRef := func(s string) bool {
		return s != "" && len(s) <= 4096 && validCredential(s) && !strings.ContainsAny(s, "\x00\r\n")
	}
	switch c.Transport {
	case "stdio":
		if c.Command == "" || len(c.Command) > 4096 || c.URL != "" || c.Credential != "" || len(c.Headers) > 0 || len(c.Args) > 128 || len(c.Env) > 128 {
			return fail()
		}
		if strings.ContainsAny(c.Command, "\x00\r\n") || c.Cwd != "" && (!filepath.IsAbs(c.Cwd) || filepath.Clean(c.Cwd) != c.Cwd || len(c.Cwd) > 4096) {
			return fail()
		}
		for _, arg := range c.Args {
			if len(arg) > 16384 || strings.ContainsRune(arg, 0) {
				return fail()
			}
		}
		for key, ref := range c.Env {
			if len(key) > 128 || !mcpEnvName.MatchString(key) || !validRef(ref) {
				return fail()
			}
		}
	case "streamable-http":
		if c.Command != "" || len(c.Args) > 0 || c.Cwd != "" || len(c.Env) > 0 || len(c.Headers) > 32 || len(c.URL) > 4096 {
			return fail()
		}
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fail()
		}
		if c.Credential != "" && !validRef(c.Credential) {
			return fail()
		}
		seen := map[string]bool{}
		for key, ref := range c.Headers {
			n := http.CanonicalHeaderKey(key)
			if !mcpHeaderName.MatchString(key) || len(key) > 128 || seen[n] || !validRef(ref) {
				return fail()
			}
			seen[n] = true
			switch n {
			case "Host", "Connection", "Content-Length", "Content-Type", "Accept", "Transfer-Encoding", "Upgrade", "Proxy-Authorization", "Proxy-Connection", "Trailer", "Te", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id":
				return fail()
			}
			if n == "Authorization" && c.Credential != "" {
				return fail()
			}
		}
	default:
		return fail()
	}
	return c, nil
}

// BindingVersion excludes resolved values; changing a reference or endpoint
// changes tool references even if the advertised schema remains identical.
func (c MCPConfig) BindingVersion() string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
