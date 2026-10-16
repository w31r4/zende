// Package plugin owns independent, trusted extension processes.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const ProtocolVersion = 1
const MaxManifestBytes = 128 << 10
const InitializeTimeout = 30 * time.Second
const ShutdownTimeout = 2 * time.Second
const MaxDiagnosticBytes = 64 << 10
const MaxSecretBytes = 64 << 10
const MaxFrameBytes = 8 << 20

// Manifest contains public identity/launch settings and environment references.
// Version is the author's release identity; Digest also covers launch settings.
type Manifest struct {
	SchemaVersion    int               `json:"schema_version" yaml:"schema_version"`
	ID               string            `json:"id" yaml:"id"`
	Version          string            `json:"version" yaml:"version"`
	ProtocolVersions []int             `json:"protocol_versions" yaml:"protocol_versions"`
	Command          string            `json:"command" yaml:"command"`
	Args             []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Cwd              string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Capabilities     []string          `json:"capabilities" yaml:"capabilities"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (m Manifest) Validate() error {
	fail := protocol.E("invalid_resource", "invalid plugin manifest; use schema_version 1, bounded identity, launch settings and environment references")
	data, err := json.Marshal(m)
	if err != nil || len(data) > MaxManifestBytes || m.SchemaVersion != 1 || len(m.ID) > 64 || !namePattern.MatchString(m.ID) {
		return fail
	}
	if m.Version == "" || len(m.Version) > 128 || strings.IndexFunc(m.Version, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return fail
	}
	if len(m.ProtocolVersions) == 0 || len(m.ProtocolVersions) > 8 {
		return fail
	}
	versions := map[int]bool{}
	for _, v := range m.ProtocolVersions {
		if v < 1 || v > 65535 || versions[v] {
			return fail
		}
		versions[v] = true
	}
	if !validCapabilities(m.Capabilities) || m.Command == "" || len(m.Command) > 4096 || !utf8.ValidString(m.Command) || strings.ContainsAny(m.Command, "\x00\r\n") {
		return fail
	}
	if m.Cwd != "" && (!filepath.IsAbs(m.Cwd) || filepath.Clean(m.Cwd) != m.Cwd || len(m.Cwd) > 4096 || !utf8.ValidString(m.Cwd) || strings.ContainsRune(m.Cwd, 0)) {
		return fail
	}
	if len(m.Args) > 128 || len(m.Env) > 128 {
		return fail
	}
	for _, arg := range m.Args {
		if len(arg) > 16384 || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fail
		}
	}
	for key, ref := range m.Env {
		if len(key) > 128 || !envPattern.MatchString(key) || len(ref) > 4096 || !utf8.ValidString(ref) || strings.ContainsAny(ref, "\x00\r\n") {
			return fail
		}
		switch {
		case strings.HasPrefix(ref, "env:") && envPattern.MatchString(strings.TrimPrefix(ref, "env:")):
		case strings.HasPrefix(ref, "file:") && filepath.IsAbs(strings.TrimPrefix(ref, "file:")):
		default:
			return fail
		}
	}
	return nil
}

func validCapabilities(caps []string) bool {
	if len(caps) == 0 || len(caps) > 5 {
		return false
	}
	seen := map[string]bool{}
	for _, c := range caps {
		if seen[c] || !slices.Contains([]string{"tools", "providers", "commands", "resources", "events"}, c) {
			return false
		}
		seen[c] = true
	}
	return true
}

func (m Manifest) clone() Manifest {
	raw, _ := json.Marshal(m)
	var out Manifest
	_ = json.Unmarshal(raw, &out)
	return out
}

func (m Manifest) Digest() string {
	m = m.clone()
	slices.Sort(m.Capabilities)
	slices.Sort(m.ProtocolVersions)
	raw, _ := json.Marshal(m)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

type Identity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Limits struct {
	InitializeMS       int `json:"initialize_ms"`
	ShutdownMS         int `json:"shutdown_ms"`
	FrameBytes         int `json:"frame_bytes"`
	DiagnosticBytes    int `json:"diagnostic_bytes"`
	ConcurrentRequests int `json:"concurrent_requests"`
	QueuedProgress     int `json:"queued_progress"`
	ProgressBytes      int `json:"progress_bytes"`
	CancelMS           int `json:"cancel_ms"`
	WriteMS            int `json:"write_ms"`
	Definitions        int `json:"definitions"`
	CatalogBytes       int `json:"catalog_bytes"`
	DefinitionBytes    int `json:"definition_bytes"`
	ArgumentBytes      int `json:"argument_bytes"`
	ResultBytes        int `json:"result_bytes"`
	ProviderModels     int `json:"provider_models"`
}

func DefaultLimits() Limits {
	return Limits{InitializeMS: int(InitializeTimeout.Milliseconds()), ShutdownMS: int(ShutdownTimeout.Milliseconds()), FrameBytes: MaxFrameBytes, DiagnosticBytes: MaxDiagnosticBytes,
		ConcurrentRequests: MaxConcurrentRequests, QueuedProgress: MaxQueuedProgress, ProgressBytes: MaxProgressBytes, CancelMS: int(CancelTimeout.Milliseconds()), WriteMS: int(WriteTimeout.Milliseconds()),
		Definitions: MaxDefinitions, CatalogBytes: MaxCatalogBytes, DefinitionBytes: MaxDefinitionBytes, ArgumentBytes: MaxArgumentBytes, ResultBytes: MaxResultBytes, ProviderModels: MaxProviderModels}
}

type InitializeParams struct {
	ProtocolVersions []int    `json:"protocol_versions"`
	Host             Identity `json:"host"`
	Plugin           Identity `json:"plugin"`
	InstanceID       string   `json:"instance_id"`
	Capabilities     []string `json:"capabilities"`
	Limits           Limits   `json:"limits"`
}

type InitializeResult struct {
	ProtocolVersion int      `json:"protocol_version"`
	Plugin          Identity `json:"plugin"`
	Capabilities    []string `json:"capabilities"`
}
