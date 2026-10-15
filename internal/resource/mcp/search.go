package mcp

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

const (
	DefaultSearchLimit = 8
	MaxSearchLimit     = 50
	DefaultSearchBytes = 64 << 10
	MaxSearchBytes     = 1 << 20
	MaxQueryBytes      = 1024
)

type SearchRequest struct {
	Query    string `json:"query"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type SearchResult struct {
	Version         string       `json:"catalog_version"`
	ResourceVersion string       `json:"resource_version,omitempty"`
	Generation      int64        `json:"generation,omitempty"`
	Tools           []Definition `json:"tools"`
	NextCursor      string       `json:"next_cursor,omitempty"`
}

type searchCursor struct {
	Version string `json:"v"`
	Query   string `json:"q"`
	Offset  int    `json:"n"`
}

// Search has deterministic lexical ranking, complete definitions, and a byte
// budget covering the entire JSON result. A too-large first hit is an error,
// not a truncated schema or a cursor that silently skips it.
func (req SearchRequest) Validate() error {
	if len(req.Query) > MaxQueryBytes || !utf8.ValidString(req.Query) || strings.ContainsRune(req.Query, 0) || len(req.Cursor) > 512 {
		return protocol.E("invalid_params", "MCP search query or cursor is invalid")
	}
	if req.Limit < 0 || req.Limit > MaxSearchLimit || req.MaxBytes != 0 && (req.MaxBytes < 1024 || req.MaxBytes > MaxSearchBytes) {
		return protocol.E("invalid_params", "MCP search limit must be 1..50 and max_bytes 1024..1048576")
	}
	return nil
}

func (s *Snapshot) Search(req SearchRequest) (SearchResult, error) {
	return s.search(req, SearchResult{})
}

// SearchPublished includes durable provenance in the same result byte budget.
func (s *Snapshot) SearchPublished(req SearchRequest, version string, generation int64) (SearchResult, error) {
	if len(version) != 64 || generation <= 0 {
		return SearchResult{}, protocol.E("invalid_state", "MCP catalog has no publication")
	}
	return s.search(req, SearchResult{ResourceVersion: version, Generation: generation})
}

func (s *Snapshot) search(req SearchRequest, result SearchResult) (SearchResult, error) {
	if err := req.Validate(); err != nil {
		return SearchResult{}, err
	}
	if req.Limit == 0 {
		req.Limit = DefaultSearchLimit
	}
	if req.MaxBytes == 0 {
		req.MaxBytes = DefaultSearchBytes
	}
	query := strings.Join(strings.Fields(strings.ToLower(req.Query)), " ")
	queryHash := digest(query)
	offset := 0
	if req.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		var cursor searchCursor
		if err != nil || protocol.StrictDecode(b, &cursor) != nil || cursor.Version != s.version || cursor.Query != queryHash || cursor.Offset < 0 {
			return SearchResult{}, protocol.E("stale_cursor", "MCP search cursor is invalid or outdated; restart search")
		}
		offset = cursor.Offset
	}
	type match struct{ index, score int }
	matches := []match{}
	for i, e := range s.entries {
		name, title, description := strings.ToLower(e.name), strings.ToLower(e.title), strings.ToLower(e.description)
		score, found := 0, true
		for _, word := range strings.Fields(query) {
			switch {
			case name == word:
				score += 100
			case strings.Contains(name, word):
				score += 10
			case strings.Contains(title, word):
				score += 5
			case strings.Contains(description, word):
				score++
			default:
				found = false
			}
		}
		if found {
			matches = append(matches, match{i, score})
		}
	}
	slices.SortStableFunc(matches, func(a, b match) int { return b.score - a.score })
	if offset > len(matches) {
		return SearchResult{}, protocol.E("stale_cursor", "MCP search cursor is outside the result set; restart search")
	}
	result.Version, result.Tools = s.version, []Definition{}
	for i := offset; i < len(matches) && len(result.Tools) < req.Limit; i++ {
		previousCursor := result.NextCursor
		result.Tools = append(result.Tools, copyDefinition(s.entries[matches[i].index].definition))
		result.NextCursor = ""
		if i+1 < len(matches) {
			b, _ := json.Marshal(searchCursor{s.version, queryHash, i + 1})
			result.NextCursor = base64.RawURLEncoding.EncodeToString(b)
		}
		b, _ := json.Marshal(result)
		if len(b) > req.MaxBytes {
			result.Tools = result.Tools[:len(result.Tools)-1]
			result.NextCursor = previousCursor
			if len(result.Tools) == 0 {
				return SearchResult{}, protocol.E("budget_exceeded", "MCP definition exceeds search budget; increase max_bytes")
			}
			break
		}
	}
	return result, nil
}
