package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"zende.local/harness/internal/protocol"
)

// SourceObservation contains no source bytes or external diagnostics. A failed
// read has a stable fingerprint too, so unchanged broken files do not cause an
// unbounded stream of failed preparations. Transient reads are retried instead.
type SourceObservation struct {
	Version string
	Failure string
}

func observationDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SourceVersion deliberately excludes negotiated catalogs. Observing files
// must not reconnect an unchanged MCP server or restart a plugin.
func SourceVersion(kind, source string, files []File) string {
	ordered := append([]File(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	return observationDigest(struct {
		Kind, Source string
		Files        []File
	}{kind, source, ordered})
}

func failedObservation(ctx context.Context, scope string, err error) (SourceObservation, error) {
	if ctx.Err() != nil {
		return SourceObservation{}, ctx.Err()
	}
	kind := "unavailable"
	var known *protocol.Error
	if errors.As(err, &known) {
		if known.Data.Kind == "resource_changed" {
			return SourceObservation{}, err
		}
		switch known.Data.Kind {
		case "invalid_resource", "invalid_config", "resource_limit":
			kind = known.Data.Kind
		}
	}
	return SourceObservation{Version: observationDigest([]string{scope, kind}), Failure: kind}, nil
}

// ObserveSource reads bounded local files only. Instruction readers also check
// their file set and syntax; remote initialization belongs to preparation.
func ObserveSource(ctx context.Context, kind, source string) (SourceObservation, error) {
	identity, err := Identity(kind, source)
	if err != nil {
		return SourceObservation{}, err
	}
	var files []File
	switch kind {
	case "mcp":
		var file File
		file, err = readMCPFile(ctx, identity.Source)
		files = []File{file}
	case "plugin":
		var raw []byte
		raw, err = readPluginManifest(ctx, identity.Source)
		files = []File{{Path: filepath.Base(identity.Source), Data: raw}}
	default:
		var p Package
		p, err = Prepare(ctx, kind, identity.Source)
		files = p.Files
	}
	if err != nil {
		return failedObservation(ctx, identity.ID, err)
	}
	return SourceObservation{Version: SourceVersion(kind, identity.Source, files)}, ctx.Err()
}

func ConfigSourceVersion(documents []ConfigDocument) string {
	return observationDigest(documents)
}

// ObserveConfig watches the three file layers, not credentials or the process
// environment. Explicit refresh remains the retry path for external readiness.
func ObserveConfig(ctx context.Context, paths Paths, cwd string) (SourceObservation, error) {
	var documents []ConfigDocument
	for _, source := range []string{filepath.Join(paths.Config, "config.yaml"), filepath.Join(cwd, ".zende", "config.yaml"), filepath.Join(cwd, ".zende", "config.local.yaml")} {
		document, _, err := readConfigDocument(ctx, source)
		if err != nil {
			return failedObservation(ctx, source, err)
		}
		documents = append(documents, document)
	}
	return SourceObservation{Version: ConfigSourceVersion(documents)}, ctx.Err()
}
