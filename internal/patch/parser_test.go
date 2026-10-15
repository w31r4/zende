package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPatchParserRejectsMalformedAndBoundedInputs(t *testing.T) {
	for _, text := range []string{
		"", "*** Begin Patch\n*** End Patch\n", "```\n" + patchText("*** Add File: a\n+x\n") + "```",
		patchText("*** Add File: \n+x\n"), patchText("*** Add File: a\nraw\n"),
		patchText("*** Delete File: a\n-x\n"), patchText("*** Update File: a\n"),
		patchText("*** Update File: a\n@@\n unchanged\n"), patchText("*** Update File: a\n@@\n"),
		patchText("*** Update File: a\n*** Move to: \n"), patchText("*** Update File: a\n@@\n-a\n+b\n*** End of File\n@@\n+c\n"),
		patchText("*** Add File: a\n+x\x00\n"), patchText("*** Add File: a\n+\xff\n"),
		patchText("*** Add File: a\x1b\n+x\n"), patchText("*** Add File: a \n+x\n"),
		strings.Repeat("x", MaxPatchBytes+1),
		patchText(strings.Repeat("*** Add File: a\n+x\n", MaxFiles+1)),
		patchText("*** Update File: a\n" + strings.Repeat("@@\n+x\n", MaxHunks+1)),
	} {
		if _, err := parse(text); err == nil {
			t.Fatalf("accepted invalid patch %.100q", text)
		}
	}
	text := strings.ReplaceAll(patchText("*** Add File: windows\n+a\n"), "\n", "\r\n")
	if _, err := parse(text); err != nil {
		t.Fatal(err)
	}
}

func TestPatchHunksMatchExactlyAndPreserveLineEndings(t *testing.T) {
	for _, tt := range []struct{ name, source, body, want string }{
		{"two hunks", "a\nb\nc\nd\n", "@@\n-b\n+B\n@@\n-d\n+D\n", "a\nB\nc\nD\n"},
		{"anchored duplicate", "first\nx\nsecond\nx\n", "@@ second\n-x\n+X\n", "first\nx\nsecond\nX\n"},
		{"end anchor", "x\nx\n", "@@\n-x\n+X\n*** End of File\n", "x\nX\n"},
		{"append", "a", "@@\n+b\n", "a\nb"},
		{"empty append", "", "@@\n+a\n", "a\n"},
		{"deletion", "a\nb\n", "@@\n-a\n-b\n", ""},
		{"no first marker", "a\n", "-a\n+b\n", "b\n"},
		{"CRLF", "a\r\nb\r\n", "@@\n-a\n+A\n b\n", "A\r\nb\r\n"},
		{"mixed context", "a\r\nb\nc\r\n", "@@\n a\n-b\n+B\n c\n", "a\r\nB\nc\r\n"},
		{"delete unterminated tail", "a\nb", "@@\n-b\n", "a\n"},
		{"empty line", "a\n\nb\n", "@@\n a\n-\n+middle\n b\n", "a\nmiddle\nb\n"},
		{"unicode", "中文\n猫\n", "@@\n-中文\n+绿色\n 猫\n", "绿色\n猫\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops, err := parse(patchText("*** Update File: a\n" + tt.body))
			if err != nil {
				t.Fatal(err)
			}
			got, spans, err := updateText(tt.source, "a", ops[0].chunks)
			if err != nil || got != tt.want || len(spans) != len(ops[0].chunks) {
				t.Fatalf("got %q spans=%+v err=%v want %q", got, spans, err, tt.want)
			}
		})
	}
	for _, body := range []string{
		"@@\n-a\n+b\n",        // not a suffix, End of File must not fall back
		"@@ absent\n-a\n+b\n", // absent anchor
	} {
		ops, _ := parse(patchText("*** Update File: file\n" + body + "*** End of File\n"))
		if _, _, err := updateText("a\nother\n", "file", ops[0].chunks); err == nil {
			t.Fatal("invalid anchor accepted")
		}
	}
	ops, _ := parse(patchText("*** Update File: file\n@@\n-a\n+b\n"))
	if _, _, err := updateText(" a \n", "file", ops[0].chunks); err == nil {
		t.Fatal("silently matched trimmed text")
	}
}

func TestPatchLimitsAndSpecialFilesFailBeforeWriting(t *testing.T) {
	root := t.TempDir()
	put(t, root, "large", strings.Repeat("a", MaxFileBytes+1))
	put(t, root, "lines", strings.Repeat("\n", maxLines+1))
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"large", "lines", "fifo"} {
		r, err := New().Apply(context.Background(), root, patchText("*** Add File: untouched\n+x\n*** Delete File: "+name+"\n"))
		if err == nil || r.Status != "rejected" {
			t.Fatalf("%s: %+v %v", name, r, err)
		}
		missing(t, filepath.Join(root, "untouched"))
	}
	// A large/deep path report is rejected before creating any parent directory.
	path := strings.Repeat("segment/", 120) + "file"
	r, err := New().Apply(context.Background(), root, patchText("*** Add File: "+path+"\n+x\n"))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "patch_limit" || r.Status != "rejected" {
		t.Fatalf("%+v %v", r, err)
	}
	missing(t, filepath.Join(root, "segment"))
}

func TestStagingRechecksConcurrentEditsAndNoClobberPublish(t *testing.T) {
	for _, create := range []bool{false, true} {
		root := t.TempDir()
		path := filepath.Join(root, "file")
		body := "*** Add File: file\n+new\n"
		if !create {
			put(t, root, "file", "old\n")
			body = "*** Update File: file\n@@\n-old\n+new\n"
		}
		e := New()
		original := e.createTemp
		e.createTemp = func(dir, prefix string) (*os.File, error) {
			put(t, root, "file", "external\n")
			return original(dir, prefix)
		}
		r, err := e.Apply(context.Background(), root, patchText(body))
		if err == nil || r.Files[0].DestinationWritten {
			t.Fatalf("%+v %v", r, err)
		}
		wantText(t, path, "external\n")
	}
	root := t.TempDir()
	e := New()
	pub := e.publish
	e.publish = func(temp, target string, replace bool) error {
		put(t, root, "file", "concurrent\n")
		return pub(temp, target, replace)
	}
	r, err := e.Apply(context.Background(), root, patchText("*** Add File: file\n+new\n"))
	if err == nil || r.Files[0].DestinationWritten {
		t.Fatalf("%+v %v", r, err)
	}
	wantText(t, filepath.Join(root, "file"), "concurrent\n")
}

func FuzzParsePatchNeverPanics(f *testing.F) {
	for _, seed := range []string{patchText("*** Add File: a\n+b\n"), patchText("*** Update File: a\n@@\n-a\n+b\n"), "", "*** Begin Patch"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ops, err := parse(text)
		if err != nil {
			return
		}
		for _, op := range ops {
			if op.kind == "update" {
				_, _, _ = updateText("a\nb\na\n", op.path, op.chunks)
			}
		}
	})
}
