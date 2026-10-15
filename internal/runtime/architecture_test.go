package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCoreDoesNotDependOnUI(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "zende.local/harness/internal/runtime", "zende.local/harness/internal/model", "zende.local/harness/internal/tool", "zende.local/harness/internal/process", "zende.local/harness/internal/resource", "zende.local/harness/internal/protocol", "zende.local/harness/internal/store", "zende.local/harness/internal/workspace")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	for _, dep := range []string{"charm.land/bubbletea", "charm.land/bubbles", "internal/render"} {
		if strings.Contains(string(out), dep) {
			t.Fatalf("core depends on %s", dep)
		}
	}
}
