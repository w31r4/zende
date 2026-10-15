package resource

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

const skillDocument = "---\nname: test-skill\ndescription: 中文说明\n---\nRead the complete instructions.\n"

func TestInstructionFrontmatter(t *testing.T) {
	for _, input := range []string{skillDocument, "\ufeff" + skillDocument, strings.ReplaceAll(skillDocument, "\n", "\r\n"), strings.Replace(skillDocument, "description:", "metadata: {author: local, tags: [one, two]}\ndisable-model-invocation: true\ndescription:", 1)} {
		name, description, body, disabled, err := parseInstructions([]byte(input))
		if err != nil || name != "test-skill" || description != "中文说明" || !strings.Contains(body, "Read the complete") || disabled != strings.Contains(input, "disable-model-invocation: true") {
			t.Fatal(name, description, body, disabled, err)
		}
	}
	for _, input := range []string{
		"", "No metadata", "---\nname: a\ndescription: d\nbody", "---\nname: a\ndescription: d\n---\n  \n",
		strings.Replace(skillDocument, "name: test-skill", "name: 123", 1),
		strings.Replace(skillDocument, "name: test-skill", "name: Bad_Name", 1),
		strings.Replace(skillDocument, "name: test-skill", "name: "+strings.Repeat("a", 65), 1),
		strings.Replace(skillDocument, "name: test-skill", "name: a--b", 1),
		strings.Replace(skillDocument, "description: 中文说明", "description: []", 1),
		strings.Replace(skillDocument, "description: 中文说明", "description: "+strings.Repeat("中", 1025), 1),
		strings.Replace(skillDocument, "description:", "name: duplicate\ndescription:", 1),
		strings.Replace(skillDocument, "description:", "metadata: {x: 1, x: 2}\ndescription:", 1),
		strings.Replace(skillDocument, "description:", "metadata: &anchor {x: 1}\ndescription:", 1),
		strings.Replace(skillDocument, "description:", "metadata: {1: value}\ndescription:", 1),
		strings.Replace(skillDocument, "description:", "disable-model-invocation: 'true'\ndescription:", 1),
		skillDocument + "\x00", skillDocument + string([]byte{255}), skillDocument + strings.Repeat("x", protocol.MaxInstructionBytes),
	} {
		if _, _, _, _, err := parseInstructions([]byte(input)); err == nil {
			t.Fatalf("accepted invalid instructions: %.100q", input)
		}
	}
}

func TestPackageContentIdentityAndOwnedBytes(t *testing.T) {
	files := []File{{Path: "assets/raw.bin", Data: []byte{0, 255}}, {Path: "SKILL.md", Data: []byte(skillDocument)}, {Path: "scripts/work.sh", Data: []byte("echo data"), Executable: true}}
	build := func(source string, input []File) Package {
		t.Helper()
		p, err := BuildPackage("skill", source, "SKILL.md", input)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := build("/skills/test/SKILL.md", files)
	if reordered := build(p.Info.Source, []File{files[2], files[1], files[0]}); !reflect.DeepEqual(p, reordered) {
		t.Fatal("order affected identity")
	}
	otherSource := build("/elsewhere/SKILL.md", files)
	if otherSource.Info.ID == p.Info.ID || otherSource.Info.Version != p.Info.Version {
		t.Fatal("source/content identity conflated")
	}
	files[0].Data[0] = 1
	if p.Files[1].Path != "assets/raw.bin" || p.Files[1].Data[0] != 0 || build(p.Info.Source, files).Info.Version == p.Info.Version {
		t.Fatal("asset bytes not owned/versioned")
	}
	files[0].Data[0] = 0
	files[2].Executable = false
	if build(p.Info.Source, files).Info.Version == p.Info.Version {
		t.Fatal("execute bit not versioned")
	}
	content, err := p.Content("")
	if err != nil || content.Text != skillDocument || content.Version != p.Info.Version || len(content.Entries) != 3 {
		t.Fatal(content, err)
	}
	binary, err := p.Content("assets/raw.bin")
	if err != nil || binary.Encoding != "base64" || !bytes.Equal(binary.Data, []byte{0, 255}) {
		t.Fatal(binary, err)
	}
	for _, path := range []string{"/absolute", "../escape", "a/../b", "./SKILL.md", "a\\b", "a\x00b", "missing"} {
		if _, err := p.Content(path); err == nil {
			t.Fatal("accepted path", path)
		}
	}
	for _, invalid := range [][]File{nil, {files[1], files[1]}, {{Path: "../SKILL.md", Data: []byte(skillDocument)}}, {files[1], {Path: "big", Data: make([]byte, protocol.MaxResourceFileBytes+1)}}} {
		if _, err := BuildPackage("skill", p.Info.Source, "SKILL.md", invalid); err == nil {
			t.Fatal("accepted invalid file set")
		}
	}
	if _, err := BuildPackage("skill", "/bad\x00/SKILL.md", "SKILL.md", []File{files[1]}); err == nil {
		t.Fatal("accepted NUL source")
	}
}

func TestPrepareCapturesFilesWithoutRunningScripts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", skillDocument, 0600)
	marker := filepath.Join(dir, "must-not-exist")
	write("run.sh", "#!/bin/sh\ntouch '"+marker+"'\n", 0700)
	if err := os.Symlink("SKILL.md", filepath.Join(dir, "reference.md")); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(context.Background(), "skill", dir)
	if err != nil || p.Info.Files != 3 {
		t.Fatal(p.Info, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("script executed", err)
	}
	if err := os.Remove(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatal(err)
	}
	if content, err := p.Content("run.sh"); err != nil || !strings.Contains(content.Text, marker) {
		t.Fatal("snapshot followed changed files", content, err)
	}
	if _, err := Prepare(context.Background(), "prompt", filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(cancelled, "skill", dir); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestPrepareRejectsNonFilesEscapingLinksAndLimits(t *testing.T) {
	for _, kind := range []string{"outside", "cycle", "directory-link", "fifo", "depth", "count", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillDocument), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "outside":
				err = os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(dir, "bad"))
			case "cycle":
				err = os.Symlink("bad", filepath.Join(dir, "bad"))
			case "directory-link":
				err = os.Symlink(".", filepath.Join(dir, "bad"))
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(dir, "bad"), 0600)
			case "depth":
				err = os.MkdirAll(filepath.Join(dir, strings.Repeat("d/", 17)), 0700)
			case "count":
				for i := range protocol.MaxResourceFiles {
					if err = os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); err != nil {
						break
					}
				}
			case "bytes":
				for i := range 8 {
					if err = os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), make([]byte, protocol.MaxResourceFileBytes), 0600); err != nil {
						break
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := Prepare(context.Background(), "skill", dir); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted invalid resource", kind)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("resource preparation blocked", kind)
			}
		})
	}
}
