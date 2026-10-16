package plugin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"time"

	"zende.local/harness/internal/protocol"
)

const ResourceReadTimeout = 30 * time.Second

// ResourceFile is data, including when its executable bit is set. The host
// never writes or executes a plugin resource as part of discovery or reading.
type ResourceFile struct {
	Path       string `json:"path"`
	Data       []byte `json:"data"` // base64 on the wire
	Executable bool   `json:"executable"`
}

type ResourceFileRead struct {
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

type ResourceFileResult struct {
	Ref string `json:"ref"`
	ResourceFile
}

// ValidateResourceFiles also applies when restoring a saved bundle. Ref
// ownership is checked against the parent's catalog at publication time.
func ValidateResourceFiles(d ResourceDefinition, files []ResourceFile) error {
	if validResource(d.Resource) != nil || len(files) != len(d.Files) {
		return protocol.E("invalid_resource", "plugin resource file set differs from its catalog")
	}
	seen := map[string]bool{}
	for _, f := range files {
		n := slices.IndexFunc(d.Files, func(info protocol.ResourceFileInfo) bool { return info.Path == f.Path })
		if n < 0 || seen[f.Path] || !matchesResourceFile(d.Files[n], f) {
			return protocol.E("invalid_resource", "plugin resource bytes or attributes differ from its catalog")
		}
		seen[f.Path] = true
	}
	return nil
}

func matchesResourceFile(info protocol.ResourceFileInfo, f ResourceFile) bool {
	return info.Path == f.Path && info.Bytes == int64(len(f.Data)) && info.Executable == f.Executable && info.SHA256 == fmt.Sprintf("%x", sha256.Sum256(f.Data))
}

// ReadResource pins the discovered definition and reads one file per frame.
// An 8 MiB bundle can exceed the 8 MiB frame budget after base64 encoding.
func (i *Instance) ReadResource(ctx context.Context, ref string) (ResourceDefinition, []ResourceFile, error) {
	i.mu.Lock()
	catalog, ready := i.catalog, i.info.State == "ready"
	i.mu.Unlock()
	if !ready || catalog == nil {
		return ResourceDefinition{}, nil, protocol.E("unavailable", "plugin resource catalog is unavailable")
	}
	definitions := catalog.Resources()
	n := slices.IndexFunc(definitions, func(d ResourceDefinition) bool { return d.Ref == ref })
	if n < 0 {
		return ResourceDefinition{}, nil, protocol.E("stale_reference", "plugin resource changed; inspect the current catalog")
	}
	if err := ctx.Err(); err != nil {
		return ResourceDefinition{}, nil, err
	}
	d := definitions[n]
	// A client cancellation abandons the read, not unrelated plugin calls. The
	// in-flight reply is drained within a fixed budget; the next file is skipped.
	// A plugin that never replies still fails the connection on timeout.
	type result struct {
		files []ResourceFile
		err   error
	}
	done := make(chan result, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ResourceReadTimeout)
		defer cancel()
		var files []ResourceFile
		for _, info := range d.Files {
			if err := ctx.Err(); err != nil {
				done <- result{err: err}
				return
			}
			raw, err := i.exchange(readCtx, "plugin.resource.read", ResourceFileRead{Ref: ref, Path: info.Path})
			if err != nil {
				done <- result{err: err}
				return
			}
			var reply struct {
				Ref        string `json:"ref"`
				Path       string `json:"path"`
				Data       []byte `json:"data"`
				Executable *bool  `json:"executable"`
			}
			if len(raw) > 2<<20 || protocol.StrictDecode(raw, &reply) != nil || reply.Executable == nil || reply.Data == nil || reply.Ref != ref {
				done <- result{err: protocol.E("invalid_resource", "plugin resource file differs from its catalog")}
				return
			}
			file := ResourceFile{Path: reply.Path, Data: reply.Data, Executable: *reply.Executable}
			if !matchesResourceFile(info, file) {
				done <- result{err: protocol.E("invalid_resource", "plugin resource file differs from its catalog")}
				return
			}
			files = append(files, file)
		}
		done <- result{files: files}
	}()
	select {
	case r := <-done:
		return d, r.files, r.err
	case <-ctx.Done():
		return ResourceDefinition{}, nil, ctx.Err()
	}
}
