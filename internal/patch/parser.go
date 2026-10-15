// Package patch implements the default text-edit protocol independently of the
// model, terminal and Git. Parsing and preparation never write to the workspace.
package patch

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPatchBytes = 1 << 20
const MaxFiles = 128
const MaxHunks = 1024
const MaxFileBytes = 8 << 20
const MaxTotalBytes = 32 << 20
const maxLines = 200000

type Error struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	where := e.Path
	if e.Line > 0 {
		where += fmt.Sprintf(":%d", e.Line)
	}
	if where != "" {
		return where + ": " + e.Message
	}
	return e.Message
}

func problem(code, path string, line int, message string) *Error {
	return &Error{Code: code, Path: path, Line: line, Message: message}
}

type operation struct {
	kind, path, move string
	line             int
	added            []string
	chunks           []chunk
}
type chunk struct {
	anchor string
	line   int
	eof    bool
	lines  []string // first byte is space, + or -
}

// parse accepts literal patch text, not shell heredoc wrappers or Markdown.
func parse(text string) ([]operation, error) {
	if len(text) > MaxPatchBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return nil, problem("invalid_patch", "", 0, "patch must be UTF-8 text without NUL, at most 1 MiB")
	}
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil, problem("invalid_patch", "", 1, "expected Begin Patch, at least one file, and End Patch")
	}
	var ops []operation
	hunks := 0
	for i := 1; i < len(lines)-1; {
		op := operation{line: i + 1}
		for _, header := range []struct{ prefix, kind string }{{"*** Add File: ", "add"}, {"*** Delete File: ", "delete"}, {"*** Update File: ", "update"}} {
			if strings.HasPrefix(lines[i], header.prefix) {
				op.kind, op.path = header.kind, strings.TrimPrefix(lines[i], header.prefix)
				break
			}
		}
		if op.kind == "" || !validPath(op.path) {
			return nil, problem("invalid_patch", "", i+1, "expected a file header with a nonempty path (at most 4096 bytes, no control characters)")
		}
		i++
		if op.kind == "update" && i < len(lines)-1 && strings.HasPrefix(lines[i], "*** Move to: ") {
			op.move = strings.TrimPrefix(lines[i], "*** Move to: ")
			if !validPath(op.move) {
				return nil, problem("invalid_patch", op.path, i+1, "invalid move destination")
			}
			i++
		}
		for i < len(lines)-1 && !fileHeader(lines[i]) {
			switch op.kind {
			case "add":
				if !strings.HasPrefix(lines[i], "+") {
					return nil, problem("invalid_patch", op.path, i+1, "added lines must start with +")
				}
				op.added = append(op.added, lines[i][1:])
				i++
			case "delete":
				return nil, problem("invalid_patch", op.path, i+1, "Delete File takes no body")
			case "update":
				c := chunk{line: i + 1}
				if lines[i] == "@@" || strings.HasPrefix(lines[i], "@@ ") {
					if lines[i] != "@@" {
						c.anchor = strings.TrimPrefix(lines[i], "@@ ")
					}
					i++
				} else if len(op.chunks) > 0 {
					return nil, problem("invalid_patch", op.path, i+1, "expected @@ before the next hunk")
				}
				changed := false
				for i < len(lines)-1 && !fileHeader(lines[i]) && lines[i] != "@@" && !strings.HasPrefix(lines[i], "@@ ") {
					line := lines[i]
					i++
					if line == "*** End of File" {
						c.eof = true
						break
					}
					if line == "" || !strings.ContainsRune(" +-", rune(line[0])) {
						return nil, problem("invalid_patch", op.path, i, "hunk lines must start with space, + or -")
					}
					changed = changed || line[0] != ' '
					c.lines = append(c.lines, line)
				}
				if !changed {
					return nil, problem("invalid_patch", op.path, c.line, "hunk must add or remove text")
				}
				op.chunks = append(op.chunks, c)
				hunks++
				if hunks > MaxHunks {
					return nil, problem("patch_limit", op.path, c.line, "patch exceeds 1024 hunks")
				}
				if c.eof && i < len(lines)-1 && !fileHeader(lines[i]) {
					return nil, problem("invalid_patch", op.path, i+1, "End of File must finish this file's hunks")
				}
			}
		}
		if op.kind == "update" && len(op.chunks) == 0 && op.move == "" {
			return nil, problem("invalid_patch", op.path, op.line, "update needs a hunk or Move to")
		}
		ops = append(ops, op)
		if len(ops) > MaxFiles {
			return nil, problem("invalid_patch", "", op.line, "patch exceeds 128 files")
		}
	}
	return ops, nil
}

func fileHeader(line string) bool {
	return strings.HasPrefix(line, "*** Add File: ") || strings.HasPrefix(line, "*** Delete File: ") || strings.HasPrefix(line, "*** Update File: ")
}

func validPath(path string) bool {
	return path != "" && len(path) <= 4096 && strings.TrimSpace(path) == path && strings.IndexFunc(path, unicode.IsControl) < 0
}
