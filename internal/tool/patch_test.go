package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/patch"
)

func TestPatchToolSchemaResultsAndErrors(t *testing.T) {
	executor := PatchExecutor()
	if _, err := NewRegistry(executor); err != nil {
		t.Fatal(err)
	}
	d := executor.Descriptor()
	if d.Name != "apply_patch" || !strings.Contains(d.Description, "1024 hunks") {
		t.Fatal(d)
	}
	root := t.TempDir()
	for _, args := range []string{`{}`, `{"patch":null}`, `{"patch":"","Patch":"x"}`, `{"patch":"x","patch":"y"}`, `[]`} {
		if _, err := executor.Execute(context.Background(), Call{Cwd: root, Arguments: json.RawMessage(args)}); err == nil {
			t.Fatal("accepted", args)
		}
	}
	args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: test\n+text\n*** End Patch\n"})
	r, err := executor.Execute(context.Background(), Call{ID: "test", Cwd: root, Arguments: args})
	var report patch.Report
	if err != nil || len(r.Content) != 1 || json.Unmarshal([]byte(r.Content[0].Text), &report) != nil || report.Status != "applied" {
		t.Fatal(r, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "test"))
	if string(data) != "text\n" {
		t.Fatal(string(data))
	}
	r, err = executor.Execute(context.Background(), Call{ID: "different", Cwd: root, Arguments: args})
	if err == nil || json.Unmarshal([]byte(r.Content[0].Text), &report) != nil || report.Status != "rejected" {
		t.Fatal(r, err)
	}
}

func TestPatchResultFitsModelBudget(t *testing.T) {
	// Worst-case path escaping plus the maximum number of hunk spans. Apply
	// limits the sum of file and created-directory paths before writing.
	report := patch.Report{Status: "partial"}
	for i := 0; i < patch.MaxFiles; i++ {
		report.Files = append(report.Files, patch.FileResult{Path: strings.Repeat("<", 256), Kind: "update", Status: "applied", BeforeSHA256: strings.Repeat("0", 64), AfterSHA256: strings.Repeat("f", 64)})
	}
	for i := 0; i < patch.MaxHunks; i++ {
		report.Files[0].Spans = append(report.Files[0].Spans, patch.Span{OldStart: 200000, OldCount: 200000, NewStart: 200000, NewCount: 200000})
	}
	data, _ := json.Marshal(report)
	encoded, _ := json.Marshal(Result{Content: []model.Block{{Type: "text", Text: string(data)}}})
	if len(encoded) > model.MaxBlockBytes {
		t.Fatal("unretainable result", len(encoded))
	}
}
