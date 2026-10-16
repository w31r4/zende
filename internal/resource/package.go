package resource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/resource/plugin"
)

// Package is prepared outside the database transaction, then validated and
// published as one version. Scripts and assets are data, never executed here.
type Package struct {
	Info         protocol.ResourceInfo
	Instructions string
	Entry        string
	Files        []File
	MCP          *mcp.Catalog         `json:",omitempty"`
	Plugin       *plugin.SavedCatalog `json:",omitempty"`
}
type File struct {
	Path       string
	Data       []byte
	Executable bool
}

var skillName = regexp.MustCompile("^[a-z0-9]+(-[a-z0-9]+)*$")

// Identity is available before files are readable, so failed installations can
// still be inspected and retried. It performs no filesystem I/O.
func Identity(kind, source string) (protocol.ResourceInfo, error) {
	if (kind != "skill" && kind != "prompt" && kind != "mcp" && kind != "plugin") || !filepath.IsAbs(source) || len(source) > 4096 || strings.ContainsRune(source, 0) || !utf8.ValidString(source) {
		return protocol.ResourceInfo{}, protocol.E("invalid_params", "resource kind must be skill, prompt, mcp or plugin and source must be an absolute path of at most 4096 bytes")
	}
	source = filepath.Clean(source)
	if kind == "skill" && filepath.Base(source) != "SKILL.md" {
		source = filepath.Join(source, "SKILL.md")
	}
	if len(source) > 4096 {
		return protocol.ResourceInfo{}, protocol.E("invalid_params", "resource source exceeds 4096 bytes")
	}
	id := sha256.Sum256([]byte(kind + "\x00" + source))
	return protocol.ResourceInfo{ID: fmt.Sprintf("resource_%x", id[:16]), Kind: kind, Source: source}, nil
}

func parseInstructions(data []byte) (name, description, body string, disabled bool, err error) {
	fail := func() (string, string, string, bool, error) {
		return "", "", "", false, protocol.E("invalid_resource", "instructions require YAML frontmatter with name, description and a nonempty UTF-8 body")
	}
	if len(data) > protocol.MaxInstructionBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fail()
	}
	lines := strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n")
	if len(lines) < 4 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return fail()
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return fail()
	}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fail()
	}
	// Permit ecosystem metadata, but do not silently interpret its values as
	// permissions, shell commands or dependencies. Reject duplicate keys/aliases.
	var check func(*yaml.Node) bool
	check = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
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
			if !check(child) {
				return false
			}
		}
		return true
	}
	if !check(&doc) {
		return fail()
	}
	nodes := doc.Content[0].Content
	for i := 0; i < len(nodes); i += 2 {
		key, value := nodes[i].Value, nodes[i+1]
		switch key {
		case "name", "description":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return fail()
			}
			if key == "name" {
				name = value.Value
			} else {
				description = value.Value
			}
		case "disable-model-invocation":
			if value.Tag != "!!bool" || value.Decode(&disabled) != nil {
				return fail()
			}
		}
	}
	body = strings.Join(lines[end+1:], "\n")
	if !skillName.MatchString(name) || len(name) > 64 || strings.TrimSpace(description) == "" || utf8.RuneCountInString(description) > 1024 || strings.TrimSpace(body) == "" {
		return fail()
	}
	return name, description, body, disabled, nil
}

// Prepare reads twice and compares complete file sets, contents and execute bits.
// This catches writes during preparation, not a writer's unstated future edits;
// publishers should finish files before register, preferably by atomic rename.
func Prepare(ctx context.Context, kind, source string) (Package, error) {
	if kind == "mcp" || kind == "plugin" {
		return Package{}, protocol.E("invalid_params", "MCP and plugin preparation require a managed connection")
	}
	identity, err := Identity(kind, source)
	if err != nil {
		return Package{}, err
	}
	source = identity.Source
	rootPath, entry := filepath.Dir(source), filepath.Base(source)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return Package{}, protocol.E("invalid_resource", "resource directory unavailable")
	}
	defer root.Close()
	read := func() ([]File, error) {
		var files []File
		var total int64
		entries := 0
		var walk func(string, int) error
		readFile := func(name string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return protocol.E("invalid_resource", "resource file unavailable: "+name)
			}
			defer f.Close()
			before, err := f.Stat()
			if err != nil || !before.Mode().IsRegular() {
				return protocol.E("invalid_resource", "resource file must be regular: "+name)
			}
			if before.Size() > protocol.MaxResourceFileBytes || len(files) >= protocol.MaxResourceFiles {
				return protocol.E("resource_limit", "resource exceeds file count or byte limit")
			}
			data, err := io.ReadAll(io.LimitReader(f, protocol.MaxResourceFileBytes+1))
			if err != nil {
				return protocol.E("invalid_resource", "resource file unreadable: "+name)
			}
			after, err := f.Stat()
			if err != nil || len(data) != int(before.Size()) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				return protocol.E("resource_changed", "resource changed during preparation")
			}
			total += int64(len(data))
			if len(data) > protocol.MaxResourceFileBytes || total > protocol.MaxResourceBytes {
				return protocol.E("resource_limit", "resource exceeds byte limit")
			}
			files = append(files, File{Path: filepath.ToSlash(name), Data: data, Executable: before.Mode()&0111 != 0})
			return nil
		}
		walk = func(dir string, depth int) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if depth > 16 {
				return protocol.E("resource_limit", "resource directory depth exceeds 16")
			}
			f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return protocol.E("invalid_resource", "resource directory unreadable")
			}
			children, err := f.ReadDir(1025)
			f.Close()
			if err != nil && err != io.EOF {
				return protocol.E("invalid_resource", "resource directory unreadable")
			}
			entries += len(children)
			if entries > 1024 {
				return protocol.E("resource_limit", "resource has more than 1024 entries")
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if child.Name() == ".git" {
					continue
				}
				name := filepath.Join(dir, child.Name())
				if child.IsDir() {
					if err := walk(name, depth+1); err != nil {
						return err
					}
				} else if err := readFile(name); err != nil {
					return err
				}
			}
			return nil
		}
		if kind == "prompt" {
			err := readFile(entry)
			return files, err
		}
		err := walk(".", 0)
		return files, err
	}
	files, err := read()
	if err != nil {
		return Package{}, err
	}
	p, err := BuildPackage(kind, source, entry, files)
	if err != nil {
		return Package{}, err
	}
	again, err := read()
	if err != nil {
		return Package{}, err
	}
	other, err := BuildPackage(kind, source, entry, again)
	if err != nil {
		return Package{}, err
	}
	if other.Info.Version != p.Info.Version {
		return Package{}, protocol.E("resource_changed", "resource changed during preparation")
	}
	return p, nil
}

// BuildPackage revalidates persisted or plugin-supplied bytes without filesystem
// I/O. Hashes include resource paths and executable bits as well as file bytes.
func BuildPackage(kind, source, entry string, files []File) (Package, error) {
	if (kind != "skill" && kind != "prompt") || !filepath.IsAbs(source) || filepath.Clean(source) != source || len(source) > 4096 || strings.ContainsRune(source, 0) || !utf8.ValidString(source) || len(files) == 0 || len(files) > protocol.MaxResourceFiles {
		return Package{}, protocol.E("invalid_resource", "invalid package identity or file count")
	}
	if (kind == "skill" && (entry != "SKILL.md" || filepath.Base(source) != entry)) || (kind == "prompt" && (len(files) != 1 || filepath.Base(source) != entry)) {
		return Package{}, protocol.E("invalid_resource", "invalid instruction entry")
	}
	p := Package{Entry: entry, Info: protocol.ResourceInfo{Kind: kind, Source: source}}
	seen := make(map[string]bool)
	var instructions []byte
	for _, f := range files {
		if !ValidResourcePath(f.Path) || seen[f.Path] || len(f.Data) > protocol.MaxResourceFileBytes {
			return Package{}, protocol.E("invalid_resource", "invalid, duplicate or oversized package file")
		}
		seen[f.Path] = true
		p.Info.Bytes += int64(len(f.Data))
		if p.Info.Bytes > protocol.MaxResourceBytes {
			return Package{}, protocol.E("resource_limit", "resource exceeds 8 MiB")
		}
		p.Files = append(p.Files, File{Path: f.Path, Data: bytes.Clone(f.Data), Executable: f.Executable})
		if f.Path == entry {
			instructions = f.Data
		}
	}
	name, description, body, disabled, err := parseInstructions(instructions)
	if err != nil {
		return Package{}, err
	}
	p.Info.Name, p.Info.Description, p.Info.DisableModelInvocation = name, description, disabled
	p.Info.Files, p.Instructions = len(p.Files), body
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	identity, err := Identity(kind, source)
	if err != nil {
		return Package{}, err
	}
	p.Info.ID = identity.ID
	raw, err := json.Marshal(struct {
		Kind, Entry string
		Files       []File
	}{kind, entry, p.Files})
	if err != nil {
		return Package{}, err
	}
	sum := sha256.Sum256(raw)
	p.Info.Version = hex.EncodeToString(sum[:])
	return p, nil
}

func ValidResourcePath(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !path.IsAbs(name) && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsAny(name, "\x00\\") && len(name) <= 1024 && utf8.ValidString(name)
}

func (p Package) Content(name string) (protocol.ResourceContent, error) {
	if name == "" {
		name = p.Entry
	}
	if !ValidResourcePath(name) {
		return protocol.ResourceContent{}, protocol.E("invalid_params", "resource path must be a relative package file")
	}
	content := protocol.ResourceContent{ResourceInfo: p.Info, Path: name}
	for _, file := range p.Files {
		if file.Path != name {
			continue
		}
		if utf8.Valid(file.Data) && bytes.IndexByte(file.Data, 0) < 0 {
			content.Encoding, content.Text = "utf-8", string(file.Data)
		} else {
			content.Encoding, content.Data = "base64", bytes.Clone(file.Data)
		}
		if name == p.Entry {
			for _, f := range p.Files {
				sum := sha256.Sum256(f.Data)
				content.Entries = append(content.Entries, protocol.ResourceFileInfo{Path: f.Path, Bytes: int64(len(f.Data)), SHA256: hex.EncodeToString(sum[:]), Executable: f.Executable})
			}
		}
		return content, nil
	}
	return protocol.ResourceContent{}, protocol.E("not_found", "resource file not found in this version")
}
