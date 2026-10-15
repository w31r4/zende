package resource

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityIsStableBeforeInstallation(t *testing.T) {
	for _, input := range []string{"/missing/review", "/missing/../missing/review/", "/missing/review/SKILL.md"} {
		identity, err := Identity("skill", input)
		if err != nil || identity.Source != "/missing/review/SKILL.md" || identity.Version != "" {
			t.Fatal(identity, err)
		}
		pkg, err := BuildPackage("skill", identity.Source, "SKILL.md", []File{{Path: "SKILL.md", Data: []byte("---\nname: review\ndescription: Review\n---\nBody.")}})
		if err != nil || pkg.Info.ID != identity.ID {
			t.Fatal(pkg.Info, err)
		}
	}
	for _, input := range []string{"relative", "/a\x00b", "/a\xff", "/" + strings.Repeat("a", 4096), "/" + strings.Repeat("a", 4090)} {
		if _, err := Identity("skill", input); err == nil {
			t.Fatal("accepted invalid source", len(input))
		}
	}
	if _, err := Identity("unknown", "/mcp/config"); err == nil {
		t.Fatal("accepted unsupported kind")
	}
	source := filepath.Join(t.TempDir(), "prompt.md")
	identity, err := Identity("prompt", source)
	if err != nil || identity.Source != source {
		t.Fatal(identity, err)
	}
}
