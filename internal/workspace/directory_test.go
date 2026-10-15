package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
)

func TestDirectoryPagesNavigationAndLiteralPaths(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"z-last", "中文 空格", ".hidden", "a-first", "$(touch unexpected)", "-option"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": "a-first", "broken": "absent", "file-link": "file"} {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	after := ""
	for {
		page, err := Directory(context.Background(), base, protocol.DirectoryQuery{Path: ".", After: after, Limit: 2})
		if err != nil || page.Path != base || page.Parent != filepath.Dir(base) || page.Skipped != 1 || len(page.Entries) > 2 {
			t.Fatal(page, err)
		}
		for _, entry := range page.Entries {
			names = append(names, entry.Name)
			if entry.Symlink != (entry.Name == "link") {
				t.Fatal(entry)
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next <= after {
			t.Fatal("non-progressing page", page)
		}
		after = page.Next
	}
	want := []string{"$(touch unexpected)", "-option", ".hidden", "a-first", "link", "z-last", "中文 空格"}
	if !reflect.DeepEqual(names, want) {
		t.Fatal(names)
	}
	for _, path := range []string{"a-first", "link", "$(touch unexpected)", filepath.Join(base, "中文 空格")} {
		page, err := Directory(context.Background(), base, protocol.DirectoryQuery{Path: path})
		if err != nil || len(page.Entries) != 0 || page.Parent != base {
			t.Fatal(page, err)
		}
		parent, err := Directory(context.Background(), page.Path, protocol.DirectoryQuery{Path: ".."})
		if err != nil || parent.Path != base {
			t.Fatal(parent, err)
		}
	}
	root, err := Directory(context.Background(), base, protocol.DirectoryQuery{Path: "/"})
	if err != nil || root.Path != "/" || root.Parent != "" {
		t.Fatal(root, err)
	}
	now, err := os.Getwd()
	if err != nil || now != original {
		t.Fatal("browsing changed process cwd", now, err)
	}
	if _, err := os.Stat(filepath.Join(base, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("path executed", err)
	}
}

func TestDirectoryPaginationAcrossReadBatches(t *testing.T) {
	base := t.TempDir()
	for i := 350; i >= 0; i-- {
		if err := os.Mkdir(filepath.Join(base, fmt.Sprintf("dir-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	after := ""
	total := 0
	for {
		page, err := Directory(context.Background(), base, protocol.DirectoryQuery{After: after, Limit: 100})
		if err != nil || page.Skipped != 0 {
			t.Fatal(page, err)
		}
		for _, entry := range page.Entries {
			if entry.Name != fmt.Sprintf("dir-%03d", total) {
				t.Fatal("directory order or gap", total, entry)
			}
			total++
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if total != 351 {
		t.Fatal(total)
	}
}

func TestDirectoryInvalidAndDisappearingPaths(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	for _, q := range []protocol.DirectoryQuery{
		{Limit: -1}, {Limit: 101}, {After: "../"}, {After: "."}, {After: ".."},
		{After: strings.Repeat("x", 256)}, {Path: "\x00"}, {Path: string([]byte{0xff})}, {Path: strings.Repeat("x", 4097)},
	} {
		if _, err := Directory(ctx, base, q); err == nil {
			t.Fatal("invalid query accepted", q)
		}
	}
	if _, err := Directory(ctx, "relative", protocol.DirectoryQuery{}); err == nil {
		t.Fatal("relative base accepted")
	}
	path := filepath.Join(base, "gone")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Directory(ctx, base, protocol.DirectoryQuery{Path: path}); err == nil {
		t.Fatal("missing directory accepted")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Directory(ctx, base, protocol.DirectoryQuery{Path: file}); err == nil {
		t.Fatal("ordinary file accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Directory(cancelled, base, protocol.DirectoryQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Unix permits byte names that JSON cannot represent. Report the omission,
	// never substitute a different (potentially selectable) directory name.
	if err := os.Mkdir(filepath.Join(base, string([]byte{0xff})), 0700); err != nil {
		t.Fatal(err)
	}
	page, err := Directory(ctx, base, protocol.DirectoryQuery{})
	if err != nil || page.Skipped != 1 || len(page.Entries) != 0 {
		t.Fatal(page, err)
	}
}
