package resource

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const MaxResourceDirectories = 64
const MaxDiscoveryEntries = 10000
const MaxDiscoveryPage = 16

var discoveryKinds = [...]string{"skill", "prompt", "mcp", "plugin"}

func DiscoveryKinds() []string { return append([]string(nil), discoveryKinds[:]...) }

// This is a publication preflight, not a historical Config.Validate rule:
// existing snapshots remain readable if they exceed today's scanning limits.
func ValidateResourceDirectories(roots []string) error {
	if len(roots) > MaxResourceDirectories {
		return protocol.E("resource_limit", "resource_dirs exceeds 64 roots")
	}
	seen := map[string]bool{}
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || len(root) > 4096 || !utf8.ValidString(root) || strings.ContainsRune(root, 0) || seen[root] {
			return protocol.E("invalid_config", "resource_dirs requires unique clean absolute paths of at most 4096 bytes")
		}
		seen[root] = true
	}
	return nil
}

func discoveryDirectory(kind string) string {
	switch kind {
	case "skill":
		return "skills"
	case "prompt":
		return "prompts"
	case "mcp":
		return "mcp"
	case "plugin":
		return "plugins"
	}
	return ""
}

func discoveryName(kind, name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || !utf8.ValidString(name) || len(name) > 1024 || strings.ContainsAny(name, "/\x00") {
		return false
	}
	if kind == "skill" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	if kind == "prompt" {
		return ext == ".md"
	}
	return ext == ".yaml" || ext == ".yml" || ext == ".json"
}

// DiscoveryContains checks the explicit managed layout, never arbitrary file
// contents. Callers still prepare and validate the complete resource package.
func DiscoveryContains(root string, identity protocol.ResourceInfo) bool {
	directory := discoveryDirectory(identity.Kind)
	if directory == "" {
		return false
	}
	base := filepath.Join(root, directory)
	if identity.Kind == "skill" {
		parent := filepath.Dir(identity.Source)
		return filepath.Base(identity.Source) == "SKILL.md" && filepath.Dir(parent) == base && discoveryName(identity.Kind, filepath.Base(parent))
	}
	return filepath.Dir(identity.Source) == base && discoveryName(identity.Kind, filepath.Base(identity.Source))
}

type DiscoveryPage struct {
	Items []protocol.ResourceInfo
	Next  string
	More  bool
}

// DiscoverPage reads one category, retaining only limit+1 names. The scan cap
// includes ignored files, so an unrelated huge directory cannot consume an
// unlimited pass. No manifest, script, credential or connection is opened.
func DiscoverPage(ctx context.Context, root, kind, after string, limit int) (DiscoveryPage, error) {
	out := DiscoveryPage{Next: after}
	if err := ValidateResourceDirectories([]string{root}); err != nil {
		return out, err
	}
	directory := discoveryDirectory(kind)
	if directory == "" || limit < 1 || limit > MaxDiscoveryPage {
		return out, protocol.E("invalid_params", "discovery requires a supported resource kind and page limit of 1–16")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return out, protocol.E("unavailable", "resource directory cannot be opened")
	}
	defer opened.Close()
	file, err := opened.OpenFile(directory, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		return out, nil // Categories may be installed independently.
	}
	if err != nil {
		return out, protocol.E("unavailable", "resource category cannot be opened")
	}
	defer file.Close()
	var names []string
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		entries, readErr := file.ReadDir(128)
		for _, entry := range entries {
			count++
			if count > MaxDiscoveryEntries {
				return out, protocol.E("resource_limit", "resource category exceeds 10000 entries; split the directory")
			}
			name := entry.Name()
			if name <= after || !discoveryName(kind, name) || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if kind == "skill" && !entry.IsDir() || kind != "skill" && !entry.Type().IsRegular() {
				continue
			}
			at := sort.SearchStrings(names, name)
			if at < len(names) && names[at] == name {
				continue
			}
			if at > limit {
				continue
			}
			names = append(names, "")
			copy(names[at+1:], names[at:])
			names[at] = name
			if len(names) > limit+1 {
				names = names[:limit+1]
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return out, protocol.E("unavailable", "resource category cannot be read")
		}
	}
	out.More = len(names) > limit
	if out.More {
		names = names[:limit]
	}
	for _, name := range names {
		source := filepath.Join(root, directory, name)
		if kind == "skill" {
			source = filepath.Join(source, "SKILL.md")
		}
		identity, err := Identity(kind, source)
		if err != nil {
			return DiscoveryPage{}, err
		}
		out.Items = append(out.Items, identity)
		out.Next = name
	}
	return out, nil
}
