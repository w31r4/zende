// Package workspace supplies filesystem/Git queries and explicit worktree
// creation. The runtime owns durable working-directory changes and operation IDs.
package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

const maxDirectoryEntries = 100000

// DirectoryPath deliberately does not expand shell expressions or change the
// process cwd. It preserves logical symlink paths, including logical "..".
func DirectoryPath(base, path string) (string, error) {
	if !filepath.IsAbs(base) || !validPath(base) || !validPath(path) {
		return "", protocol.E("invalid_params", "directory path must be valid UTF-8 without NUL, at most 4096 bytes")
	}
	if path == "" {
		path = base
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if !validPath(path) {
		return "", protocol.E("invalid_params", "resolved path exceeds 4096 bytes")
	}
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return "", protocol.E("directory_unavailable", "path is not an accessible directory")
	}
	return path, nil
}

func validPath(path string) bool {
	return len(path) <= 4096 && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}

func Directory(ctx context.Context, base string, q protocol.DirectoryQuery) (protocol.DirectoryPage, error) {
	if err := ctx.Err(); err != nil {
		return protocol.DirectoryPage{}, err
	}
	if q.Limit < 0 || q.Limit > 100 || !validPath(q.After) || len(q.After) > 255 || strings.Contains(q.After, "/") || q.After == "." || q.After == ".." {
		return protocol.DirectoryPage{}, protocol.E("invalid_params", "invalid directory cursor or limit (1–100)")
	}
	path, err := DirectoryPath(base, q.Path)
	if err != nil {
		return protocol.DirectoryPage{}, err
	}
	// O_DIRECTORY rejects a path replaced with a FIFO/device after Stat without
	// ever blocking on opening that replacement.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return protocol.DirectoryPage{}, protocol.E("directory_unavailable", "directory cannot be read")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	page := protocol.DirectoryPage{Path: path, Entries: []protocol.DirectoryEntry{}}
	if parent := filepath.Dir(path); parent != path {
		page.Parent = parent
	}
	limit := q.Limit
	if limit == 0 {
		limit = 40
	}
	count := 0
	// Keep only the first limit+1 eligible names. Readdir order is unspecified;
	// insertion in a bounded sorted window avoids retaining the whole directory.
	for {
		if err := ctx.Err(); err != nil {
			return protocol.DirectoryPage{}, err
		}
		entries, readErr := f.ReadDir(256)
		count += len(entries)
		if count > maxDirectoryEntries {
			return protocol.DirectoryPage{}, protocol.E("directory_limit", "directory exceeds the 100000-entry scan limit; enter a child path directly")
		}
		for _, entry := range entries {
			name := entry.Name()
			if !utf8.ValidString(name) {
				page.Skipped++
				continue
			}
			symlink := entry.Type()&os.ModeSymlink != 0
			isDir := entry.IsDir()
			if symlink {
				info, err := os.Stat(filepath.Join(path, name))
				if err != nil {
					page.Skipped++
					continue
				}
				isDir = info.IsDir()
			}
			if !isDir || name <= q.After {
				continue
			}
			item := protocol.DirectoryEntry{Name: name, Symlink: symlink}
			i, found := slices.BinarySearchFunc(page.Entries, item, func(a, b protocol.DirectoryEntry) int { return strings.Compare(a.Name, b.Name) })
			if !found && i <= limit {
				page.Entries = slices.Insert(page.Entries, i, item)
				if len(page.Entries) > limit+1 {
					page.Entries = page.Entries[:limit+1]
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return protocol.DirectoryPage{}, protocol.E("directory_unavailable", "directory changed or could not be fully read")
		}
	}
	if len(page.Entries) > limit {
		page.Next = page.Entries[limit-1].Name
		page.Entries = page.Entries[:limit]
	}
	return page, nil
}
