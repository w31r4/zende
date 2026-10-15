package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource/mcp"
)

type mcpResultBody struct {
	Content    []json.RawMessage `json:"content"`
	Structured json.RawMessage   `json:"structuredContent"`
	IsError    bool              `json:"isError"`
}

func mcpDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Only retained bytes are decoded. URI-bearing parts remain data; this reader
// never resolves file:// or network resources from a server response.
func mcpPart(raw []byte, body mcpResultBody, part string) (dataOut []byte, mimeOut string, kindOut string, errOut error) {
	defer func() {
		if len(mimeOut) > 256 || mimeOut == "" || strings.ContainsAny(mimeOut, "\r\n\x00") {
			mimeOut = "application/octet-stream"
		}
		if len(kindOut) > 64 {
			kindOut = "unknown"
		}
	}()
	if part == "" || part == "result" {
		return raw, "application/json", "result", nil
	}
	if part == "structured" {
		if len(body.Structured) == 0 {
			return nil, "", "", protocol.E("not_found", "MCP result has no structured content")
		}
		return body.Structured, "application/json", "structured", nil
	}
	n, err := strconv.Atoi(part)
	if err != nil || n < 0 || n >= len(body.Content) || strconv.Itoa(n) != part {
		return nil, "", "", protocol.E("invalid_params", "part must be result, structured, or a content index")
	}
	data := body.Content[n]
	var c struct {
		Type     string  `json:"type"`
		Text     *string `json:"text"`
		Data     string  `json:"data"`
		MIME     string  `json:"mimeType"`
		Resource *struct {
			Text *string `json:"text"`
			Blob string  `json:"blob"`
			MIME string  `json:"mimeType"`
		} `json:"resource"`
	}
	if json.Unmarshal(data, &c) != nil {
		return data, "application/json", "unknown", nil
	}
	if c.Type == "text" && c.Text != nil {
		return []byte(*c.Text), "text/plain", c.Type, nil
	}
	if c.Type == "image" || c.Type == "audio" {
		if decoded, err := base64.StdEncoding.DecodeString(c.Data); err == nil {
			return decoded, c.MIME, c.Type, nil
		}
	}
	if c.Type == "resource" && c.Resource != nil {
		if c.Resource.Text != nil {
			return []byte(*c.Resource.Text), c.Resource.MIME, "resource_text", nil
		}
		if decoded, err := base64.StdEncoding.DecodeString(c.Resource.Blob); err == nil {
			return decoded, c.Resource.MIME, "resource_blob", nil
		}
	}
	return data, "application/json", c.Type, nil
}

func mcpTextPart(kind string) bool {
	return kind != "image" && kind != "audio" && kind != "resource_blob"
}

func mcpResultInfo(raw []byte) (*protocol.MCPResultInfo, error) {
	var body mcpResultBody
	if len(raw) == 0 || len(raw) > mcp.MaxResultBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &body) != nil || !json.Valid(raw) {
		return nil, protocol.E("invalid_state", "MCP result is not bounded JSON")
	}
	info := &protocol.MCPResultInfo{Bytes: len(raw), SHA256: mcpDigest(raw), IsError: body.IsError, ContentBlocks: len(body.Content), Structured: len(body.Structured) > 0, Parts: []protocol.MCPResultPart{}}
	parts := []string{}
	if info.Structured {
		parts = append(parts, "structured")
	}
	for i := 0; i < len(body.Content) && len(parts) < 8; i++ {
		parts = append(parts, strconv.Itoa(i))
	}
	info.More = len(parts) < len(body.Content)+boolInt(info.Structured)
	for _, part := range parts {
		data, mime, kind, err := mcpPart(raw, body, part)
		if err != nil {
			return nil, err
		}
		p := protocol.MCPResultPart{Part: part, Type: kind, MediaType: mime, Bytes: len(data), SHA256: mcpDigest(data)}
		if mcpTextPart(kind) && utf8.Valid(data) {
			end := min(len(data), 1024)
			for end < len(data) && !utf8.RuneStart(data[end]) {
				end--
			}
			p.Text = string(data[:end])
			p.More = end < len(data)
		} else {
			p.More = len(data) > 0
		}
		info.Parts = append(info.Parts, p)
	}
	return info, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) mcpResultDir() string { return filepath.Join(filepath.Dir(s.path), "mcp-results") }

func (s *Store) saveMCPResult(raw []byte) (*protocol.MCPResultInfo, error) {
	info, err := mcpResultInfo(raw)
	if err != nil {
		return nil, err
	}
	dir := s.mcpResultDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	// Link publishes without replacing an existing digest. Existing bytes must
	// verify too: a corrupt artifact is never silently overwritten.
	if err = os.Link(f.Name(), filepath.Join(dir, info.SHA256+".json")); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if _, err = s.readMCPResult(info); err != nil {
		return nil, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err = parent.Sync(); err != nil {
		return nil, err
	}
	return info, nil
}

func (s *Store) readMCPResult(info *protocol.MCPResultInfo) ([]byte, error) {
	bad := protocol.E("invalid_state", "retained MCP result is missing or failed integrity verification")
	digest, err := hex.DecodeString(info.SHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != info.SHA256 || info.Bytes <= 0 || info.Bytes > mcp.MaxResultBytes {
		return nil, bad
	}
	root, err := os.OpenRoot(s.mcpResultDir())
	if err != nil {
		return nil, bad
	}
	defer root.Close()
	f, err := root.OpenFile(info.SHA256+".json", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, bad
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != int64(info.Bytes) {
		return nil, bad
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(info.Bytes)+1))
	if err != nil || len(raw) != info.Bytes || mcpDigest(raw) != info.SHA256 {
		return nil, bad
	}
	return raw, nil
}

func (s *Store) ReadMCPResult(ctx context.Context, p protocol.MCPResultRead) (protocol.MCPResultPage, error) {
	var page protocol.MCPResultPage
	if p.Limit == 0 {
		p.Limit = 64 << 10
	}
	if p.Offset < 0 || p.Limit < 4 || p.Limit > 256<<10 {
		return page, protocol.E("invalid_params", "result offset must be nonnegative and limit 4–262144 bytes")
	}
	record, err := s.MCPCall(ctx, protocol.MCPCallTarget{AgentID: p.AgentID, OperationID: p.OperationID})
	if err != nil {
		return page, err
	}
	if record == nil || record.Receipt.Result == nil {
		return page, protocol.E("not_found", "MCP call has no retained result")
	}
	raw, err := s.readMCPResult(record.Receipt.Result)
	if err != nil {
		return page, err
	}
	var body mcpResultBody
	if json.Unmarshal(raw, &body) != nil {
		return page, protocol.E("invalid_state", "retained MCP result is unreadable")
	}
	data, mime, kind, err := mcpPart(raw, body, p.Part)
	if err != nil {
		return page, err
	}
	if p.Offset > len(data) {
		return page, protocol.E("invalid_params", "result offset exceeds part length")
	}
	end := p.Offset + min(p.Limit, len(data)-p.Offset)
	page = protocol.MCPResultPage{MCPCallTarget: protocol.MCPCallTarget{AgentID: p.AgentID, OperationID: p.OperationID}, Part: p.Part, MediaType: mime, Bytes: len(data), SHA256: mcpDigest(data), Offset: p.Offset}
	if page.Part == "" {
		page.Part = "result"
	}
	if mcpTextPart(kind) && utf8.Valid(data) {
		if p.Offset < len(data) && !utf8.RuneStart(data[p.Offset]) {
			return protocol.MCPResultPage{}, protocol.E("invalid_params", "text offset must be a UTF-8 character boundary")
		}
		for end < len(data) && !utf8.RuneStart(data[end]) {
			end--
		}
		page.Encoding, page.Text = "utf-8", string(data[p.Offset:end])
	} else {
		page.Encoding, page.Data = "base64", bytes.Clone(data[p.Offset:end])
	}
	page.Next, page.More = end, end < len(data)
	return page, nil
}
