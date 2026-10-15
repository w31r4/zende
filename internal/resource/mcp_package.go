package resource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
)

// BuildMCPPackage preserves configuration bytes and a complete catalog, never
// credentials or live connections. Validation does not contact the server.
func BuildMCPPackage(source string, files []File, catalog mcp.Catalog) (Package, error) {
	identity, err := Identity("mcp", source)
	if err != nil || identity.Source != source || len(files) != 1 || files[0].Path != filepath.Base(source) || !ValidResourcePath(files[0].Path) || files[0].Executable {
		return Package{}, protocol.E("invalid_resource", "invalid MCP package identity")
	}
	config, err := ParseMCPConfig(files[0].Data)
	if err != nil {
		return Package{}, err
	}
	snapshot, err := mcp.Restore(context.Background(), identity.ID, config.BindingVersion(), catalog)
	if err != nil {
		return Package{}, err
	}
	saved := snapshot.Export()
	p := Package{Info: identity, Entry: files[0].Path, Files: []File{{Path: files[0].Path, Data: bytes.Clone(files[0].Data)}}, MCP: &saved}
	p.Info.Name, p.Info.Description = config.Name, config.Description
	p.Info.Files = 1
	raw, _ := json.Marshal(saved)
	p.Info.Bytes = int64(len(files[0].Data) + len(raw))
	hash, _ := json.Marshal(struct {
		Files   []File
		Catalog mcp.Catalog
	}{p.Files, saved})
	sum := sha256.Sum256(hash)
	p.Info.Version = hex.EncodeToString(sum[:])
	return p, nil
}

func readMCPFile(ctx context.Context, source string) (File, error) {
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	f, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return File{}, protocol.E("invalid_resource", "MCP configuration file unavailable")
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return File{}, protocol.E("invalid_resource", "MCP configuration must be a regular file")
	}
	if before.Size() > MaxMCPConfigBytes {
		return File{}, protocol.E("resource_limit", "MCP configuration exceeds 128 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxMCPConfigBytes+1))
	if err != nil {
		return File{}, protocol.E("invalid_resource", "MCP configuration unreadable")
	}
	after, err := f.Stat()
	if err != nil || len(data) != int(before.Size()) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return File{}, protocol.E("resource_changed", "MCP configuration changed during preparation")
	}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	return File{Path: filepath.Base(source), Data: data}, nil
}

func PrepareMCP(ctx context.Context, source string, env func(string) string) (Package, *MCPConnection, error) {
	identity, err := Identity("mcp", source)
	if err != nil {
		return Package{}, nil, err
	}
	file, err := readMCPFile(ctx, identity.Source)
	if err != nil {
		return Package{}, nil, err
	}
	config, err := ParseMCPConfig(file.Data)
	if err != nil {
		return Package{}, nil, err
	}
	conn, err := ConnectMCP(ctx, identity, config, env)
	if err != nil {
		return Package{}, nil, err
	}
	// A change while negotiation/listing was in flight cannot publish old
	// settings as the just-installed resource. Atomic replacement is supported.
	again, err := readMCPFile(ctx, identity.Source)
	if err == nil && !bytes.Equal(file.Data, again.Data) {
		err = protocol.E("resource_changed", "MCP configuration changed during preparation")
	}
	if err != nil {
		conn.Close()
		return Package{}, nil, err
	}
	snapshot, err := conn.Snapshot()
	if err != nil {
		conn.Close()
		return Package{}, nil, err
	}
	p, err := BuildMCPPackage(identity.Source, []File{file}, snapshot.Export())
	if err != nil {
		conn.Close()
		return Package{}, nil, err
	}
	return p, conn, nil
}
