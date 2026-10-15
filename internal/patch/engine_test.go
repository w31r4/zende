package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func put(t *testing.T, root, path, text string) string {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantText(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s: got %q, %v; want %q", path, b, err, want)
	}
}

func missing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should be absent: %v", path, err)
	}
}

func patchText(body string) string { return "*** Begin Patch\n" + body + "*** End Patch\n" }

func TestAllOperationsWithoutGitPreserveTextAndMode(t *testing.T) {
	root := t.TempDir()
	updated := put(t, root, "edit", "first\r\nold\r\nlast\nunterminated")
	if err := os.Chmod(updated, 0751); err != nil {
		t.Fatal(err)
	}
	put(t, root, "delete", "delete me\n")
	put(t, root, "move", "keep\n")
	put(t, root, "move-edit", "before\n")
	text := patchText("*** Add File: nested/new\n+new\n+中文\n*** Add File: empty\n*** Update File: edit\n@@\n first\n-old\n+new\n last\n*** Delete File: delete\n*** Update File: move\n*** Move to: moved/new-name\n*** Update File: move-edit\n*** Move to: moved/edited\n@@\n-before\n+after\n")
	r, err := New().Apply(context.Background(), root, text)
	if err != nil || r.Status != "applied" || len(r.Files) != 6 || len(r.CreatedDirectories) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, f := range r.Files {
		if f.Status != "applied" || !f.Durable || (f.Kind != "delete" && !f.DestinationWritten) {
			t.Fatalf("bad file result: %+v", f)
		}
	}
	wantText(t, updated, "first\r\nnew\r\nlast\nunterminated")
	wantText(t, filepath.Join(root, "nested/new"), "new\n中文\n")
	wantText(t, filepath.Join(root, "empty"), "")
	wantText(t, filepath.Join(root, "moved/new-name"), "keep\n")
	wantText(t, filepath.Join(root, "moved/edited"), "after\n")
	missing(t, filepath.Join(root, "delete"))
	missing(t, filepath.Join(root, "move"))
	missing(t, filepath.Join(root, "move-edit"))
	missing(t, filepath.Join(root, ".git"))
	info, _ := os.Stat(updated)
	if info.Mode().Perm() != 0751 || r.Files[2].BeforeSHA256 == r.Files[2].AfterSHA256 || len(r.Files[2].Spans) != 1 {
		t.Fatalf("mode/hashes/spans: %v %+v", info.Mode(), r.Files[2])
	}
}

func TestEveryConflictIsCheckedBeforeAnyWrite(t *testing.T) {
	cases := []string{
		"*** Update File: current\n@@\n-stale\n+bad\n",
		"*** Add File: current\n+bad\n",
		"*** Update File: source\n*** Move to: current\n",
		"*** Delete File: absent\n",
		"*** Update File: duplicate\n@@\n-same\n+bad\n",
		"*** Add File: tree\n+file\n*** Add File: tree/child\n+child\n",
		"*** Update File: current\n@@\n-current\n+new\n*** Delete File: ./current\n",
		"*** Delete File: directory\n",
		"*** Update File: binary\n@@\n-a\n+b\n",
		"*** Delete File: link\n",
		"*** Add File: link\n+new\n",
	}
	for _, body := range cases {
		t.Run(strings.ReplaceAll(body, "\n", "|"), func(t *testing.T) {
			root := t.TempDir()
			current := put(t, root, "current", "current\n")
			put(t, root, "source", "source\n")
			put(t, root, "duplicate", "same\nsame\n")
			put(t, root, "binary", "a\x00b")
			put(t, root, "directory/keep", "keep")
			if err := os.Symlink(current, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			r, err := New().Apply(context.Background(), root, patchText("*** Add File: not-created/file\n+first\n"+body))
			if err == nil || r.Status != "rejected" || len(r.CreatedDirectories) != 0 {
				t.Fatalf("%+v %v", r, err)
			}
			missing(t, filepath.Join(root, "not-created"))
			wantText(t, current, "current\n")
			wantText(t, filepath.Join(root, "source"), "source\n")
		})
	}
}

func TestPreflightSnapshotAndPathChangesAreRechecked(t *testing.T) {
	for _, mutation := range []string{"content", "inode", "parent-symlink", "new-destination"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			source := put(t, root, "dir/file", "old\n")
			p, err := prepare(context.Background(), root, patchText("*** Add File: new\n+first\n*** Update File: dir/file\n@@\n-old\n+new\n"))
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "content":
				put(t, root, "dir/file", "external\n")
			case "inode":
				replacement := put(t, root, "replacement", "old\n")
				if err := os.Rename(replacement, source); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				other := t.TempDir()
				put(t, other, "file", "old\n")
				if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, filepath.Join(root, "dir")); err != nil {
					t.Fatal(err)
				}
			case "new-destination":
				put(t, root, "new", "external\n")
			}
			r, err := New().apply(context.Background(), p)
			if err == nil || r.Status != "rejected" {
				t.Fatalf("%+v %v", r, err)
			}
			if mutation == "new-destination" {
				wantText(t, filepath.Join(root, "new"), "external\n")
			} else {
				missing(t, filepath.Join(root, "new"))
			}
		})
	}
}

func TestIOFailureReportsAppliedRangeAndMoveHalf(t *testing.T) {
	t.Run("second file", func(t *testing.T) {
		root := t.TempDir()
		engine := New()
		publish := engine.publish
		engine.publish = func(temp, target string, replace bool) error {
			if filepath.Base(target) == "second" {
				return unix.EIO
			}
			return publish(temp, target, replace)
		}
		r, err := engine.Apply(context.Background(), root, patchText("*** Add File: first\n+one\n*** Add File: second\n+two\n*** Add File: third\n+three\n"))
		if err == nil || r.Status != "partial" || r.Files[0].Status != "applied" || r.Files[1].Status != "failed" || r.Files[2].Status != "not_applied" {
			t.Fatalf("%+v %v", r, err)
		}
		wantText(t, filepath.Join(root, "first"), "one\n")
		missing(t, filepath.Join(root, "second"))
		missing(t, filepath.Join(root, "third"))
		files, _ := os.ReadDir(root)
		if len(files) != 1 {
			t.Fatalf("temporary files leaked: %v", files)
		}
	})
	t.Run("move source removal", func(t *testing.T) {
		root := t.TempDir()
		source := put(t, root, "source", "old\n")
		engine := New()
		remove := engine.remove
		engine.remove = func(path string) error {
			if path == source {
				return unix.EACCES
			}
			return remove(path)
		}
		r, err := engine.Apply(context.Background(), root, patchText("*** Update File: source\n*** Move to: target\n@@\n-old\n+new\n"))
		if err == nil || r.Status != "partial" || !r.Files[0].DestinationWritten || r.Files[0].SourceRemoved || r.Files[0].Durable {
			t.Fatalf("%+v %v", r, err)
		}
		wantText(t, source, "old\n")
		wantText(t, filepath.Join(root, "target"), "new\n")
	})
	t.Run("directory sync", func(t *testing.T) {
		root := t.TempDir()
		engine := New()
		engine.syncDir = func(string) error { return unix.EIO }
		r, err := engine.Apply(context.Background(), root, patchText("*** Add File: target\n+new\n"))
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "operation_unknown" || !r.Files[0].DestinationWritten || r.Files[0].Durable {
			t.Fatalf("%+v %v", r, err)
		}
		wantText(t, filepath.Join(root, "target"), "new\n")
	})
	t.Run("created directory then staging failure", func(t *testing.T) {
		root := t.TempDir()
		engine := New()
		engine.createTemp = func(string, string) (*os.File, error) { return nil, unix.ENOSPC }
		r, err := engine.Apply(context.Background(), root, patchText("*** Add File: sub/target\n+new\n"))
		if err == nil || r.Status != "partial" || len(r.CreatedDirectories) != 1 || r.Files[0].DestinationWritten {
			t.Fatalf("%+v %v", r, err)
		}
		missing(t, filepath.Join(root, "sub/target"))
	})
}

func TestConcurrentPatchesSerializeAndCancellationPreservesFiles(t *testing.T) {
	root := t.TempDir()
	path := put(t, root, "file", "original\n")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, replacement := range []string{"one", "two"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			_, err := New().Apply(context.Background(), root, patchText("*** Update File: file\n@@\n-original\n+"+text+"\n"))
			results <- err
		}(replacement)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes %d", successes)
	}
	before, _ := os.ReadFile(path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Apply(ctx, root, patchText("*** Delete File: file\n")); err == nil {
		t.Fatal("cancelled edit succeeded")
	}
	wantText(t, path, string(before))
}

func TestAbsoluteAndSymlinkDirectoryPathsAreNotSandboxed(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	path := put(t, outside, "file", "old\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	r, err := New().Apply(context.Background(), root, patchText("*** Update File: link/file\n@@\n-old\n+new\n*** Add File: "+filepath.Join(outside, "absolute")+"\n+yes\n"))
	if err != nil || r.Files[0].Path != path {
		t.Fatalf("%+v %v", r, err)
	}
	wantText(t, path, "new\n")
	wantText(t, filepath.Join(outside, "absolute"), "yes\n")
}
