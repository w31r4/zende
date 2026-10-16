package resource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const MaxConfigLayerBytes = 1 << 20
const MaxConfigBytes = 4 << 20

// ConfigDocument records presence as well as content: creating or removing an
// override must invalidate a prepared candidate even if its merged values match.
// Raw files are not exported; a shadowed invalid credential must not leak here.
type ConfigDocument struct {
	Source  string `json:"source"`
	Present bool   `json:"present"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256,omitempty"`
}

type ConfigSnapshot struct {
	Version   string           `json:"version"`
	Effective Effective        `json:"effective"`
	Documents []ConfigDocument `json:"documents"`
}

// PreparedConfig is immutable. The publisher owns publication/rollback and
// provider readiness; this layer only prepares and rechecks configuration.
type PreparedConfig struct {
	snapshot    ConfigSnapshot
	environment []byte
	env         func(string) string
}

func (p *PreparedConfig) Snapshot() ConfigSnapshot {
	raw, _ := json.Marshal(p.snapshot)
	var copy ConfigSnapshot
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func configEnvironment(env func(string) string) ([]byte, error) {
	values := map[string]string{}
	for key, field := range map[string]string{"ZENDE_MODEL": "model", "ZENDE_EFFORT": "effort", "ZENDE_CWD": "cwd", "ZENDE_SHELL": "shell"} {
		value := env(key)
		if len(value) > MaxConfigLayerBytes || !utf8.ValidString(value) {
			return nil, protocol.E("invalid_config", "configuration environment value is invalid or exceeds the byte limit")
		}
		if value != "" {
			values[field] = value
		}
	}
	raw, _ := json.Marshal(values)
	if len(raw) > MaxConfigLayerBytes {
		return nil, protocol.E("resource_limit", "configuration environment exceeds the byte limit")
	}
	return raw, nil
}

// PrepareConfig reads user/project/local layers, composes the normal precedence
// and rereads all sources. It neither resolves credentials nor contacts models.
// Writers should publish finished files by rename; no reader can infer a future
// write from a temporarily valid document.
func PrepareConfig(ctx context.Context, paths Paths, cwd string, env func(string) string, cli, task []byte) (*PreparedConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, name := range []string{paths.Config, cwd} {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
			return nil, protocol.E("invalid_config", "configuration roots must be absolute clean paths of at most 4096 bytes")
		}
	}
	if len(cli) > MaxConfigLayerBytes || len(task) > MaxConfigLayerBytes {
		return nil, protocol.E("resource_limit", "configuration override exceeds the byte limit")
	}
	if env == nil {
		env = os.Getenv
	}
	environment, err := configEnvironment(env)
	if err != nil {
		return nil, err
	}
	layers := []Layer{}
	var documents []ConfigDocument
	for _, source := range []string{filepath.Join(paths.Config, "config.yaml"), filepath.Join(cwd, ".zende", "config.yaml"), filepath.Join(cwd, ".zende", "config.local.yaml")} {
		document, raw, err := readConfigDocument(ctx, source)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
		if document.Present {
			layers = append(layers, Layer{Name: source, YAML: raw})
		}
	}
	layers = append(layers, Layer{"environment", environment}, Layer{"cli", bytes.Clone(cli)}, Layer{"task", bytes.Clone(task)})
	effective, err := Compose(Defaults(cwd), layers...)
	if err != nil {
		var known *protocol.Error
		if errors.As(err, &known) {
			return nil, err
		}
		return nil, protocol.E("invalid_config", err.Error())
	}
	p := &PreparedConfig{snapshot: ConfigSnapshot{Effective: effective, Documents: documents}, environment: environment, env: env}
	// Version covers effective values, their provenance and every source hash.
	raw, _ := json.Marshal(p.snapshot)
	if len(raw)+sha256.Size*2 > MaxConfigBytes {
		return nil, protocol.E("resource_limit", "configuration snapshot exceeds the total byte limit")
	}
	hash := sha256.Sum256(raw)
	p.snapshot.Version = hex.EncodeToString(hash[:])
	if err := p.Verify(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// Verify is called again after slow provider/resource preparation and before
// publication. It cannot make an external filesystem write atomic with a DB
// commit; a later watcher observation prepares another complete version.
func (p *PreparedConfig) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, expected := range p.snapshot.Documents {
		actual, _, err := readConfigDocument(ctx, expected.Source)
		if err != nil {
			return err
		}
		if actual != expected {
			return protocol.E("resource_changed", "configuration sources changed during preparation; prepare again")
		}
	}
	environment, err := configEnvironment(p.env)
	if err != nil {
		return err
	}
	if !bytes.Equal(p.environment, environment) {
		return protocol.E("resource_changed", "configuration environment changed during preparation; prepare again")
	}
	return ctx.Err()
}

func readConfigDocument(ctx context.Context, source string) (ConfigDocument, []byte, error) {
	document := ConfigDocument{Source: source}
	if err := ctx.Err(); err != nil {
		return document, nil, err
	}
	f, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return document, nil, nil
	}
	if err != nil {
		return document, nil, protocol.E("invalid_config", "configuration source cannot be opened: "+source)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return document, nil, protocol.E("invalid_config", "configuration source must be a regular file: "+source)
	}
	if before.Size() > MaxConfigLayerBytes {
		return document, nil, protocol.E("resource_limit", "configuration source exceeds the byte limit: "+source)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigLayerBytes+1))
	if err != nil {
		return document, nil, protocol.E("invalid_config", "configuration source cannot be read: "+source)
	}
	if err := ctx.Err(); err != nil {
		return document, nil, err
	}
	if len(raw) > MaxConfigLayerBytes {
		return document, nil, protocol.E("resource_limit", "configuration source exceeds the byte limit: "+source)
	}
	after, err := f.Stat()
	if err != nil || before.Size() != int64(len(raw)) || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return document, nil, protocol.E("resource_changed", "configuration changed while reading: "+source)
	}
	hash := sha256.Sum256(raw)
	document.Present, document.Bytes, document.SHA256 = true, len(raw), hex.EncodeToString(hash[:])
	return document, raw, nil
}
