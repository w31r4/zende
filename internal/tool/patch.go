package tool

import (
	"context"
	"encoding/json"
	"errors"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/patch"
	"zende.local/harness/internal/protocol"
)

type patchTool struct {
	apply   func(context.Context, string, string) (patch.Report, error)
	journal string
	save    func(string, patchRecord) error
}

// PatchExecutor is the in-memory executor for engine fixtures. The core uses
// DurablePatchExecutor so effects have a receipt independent of its event log.
func PatchExecutor() Executor { return patchTool{apply: patch.New().Apply} }

func DurablePatchExecutor(directory string) Executor {
	return patchTool{apply: patch.New().Apply, journal: directory, save: savePatchRecord}
}

func (patchTool) Descriptor() Descriptor {
	return Descriptor{
		Tool: model.Tool{Name: "apply_patch", Description: `Edit UTF-8 text files with a literal patch string, relative to the Agent directory (absolute paths also work). Format:
*** Begin Patch
*** Add File: new.txt
+new content
*** Update File: existing.txt
@@
-old content
+new content
*** Update File: old-name.txt
*** Move to: new-name.txt
*** Delete File: obsolete.txt
*** End Patch
Update hunks use space for unchanged context, - for removal, + for addition; @@ exact anchor selects a preceding line. A hunk containing only additions appends. *** End of File anchors the last hunk to EOF. Old text must match exactly and uniquely; provide more context on conflicts. Add/move destinations must not exist. A move may include update hunks. All files are checked before writing; an I/O failure may leave partial changes, listed in the result with hashes and line spans. Inspect that result before retrying. Maximum patch 1 MiB, 128 files, 1024 hunks; files 8 MiB/200000 lines, combined before/after 32 MiB.`, InputSchema: json.RawMessage(`{"type":"object","properties":{"patch":{"type":"string","minLength":1}},"required":["patch"],"additionalProperties":false}`)},
		Version: "1",
		Snippet: "Apply checked text edits, additions, deletions and moves, with file-level results.",
		Guidelines: []string{
			"Use apply_patch for ordinary text edits. Read and search with commands; generators and formatters may write files directly.",
			"On a patch conflict, read the current file and revise the patch. On partial application, inspect the listed changes and continue from the actual files; do not assume rollback.",
		},
	}
}

func (t patchTool) Execute(ctx context.Context, call Call) (Result, error) {
	text, err := patchText(call)
	if err != nil {
		return Result{}, err
	}
	if t.journal != "" {
		return t.executeRecorded(ctx, call, text)
	}
	return t.applyPatch(ctx, call.Cwd, text)
}

func patchText(call Call) (string, error) {
	var p struct {
		Patch string `json:"patch"`
	}
	if err := decodeArguments(call.Arguments, &p, "patch"); err != nil {
		return "", err
	}
	return p.Patch, nil
}

func (t patchTool) applyPatch(ctx context.Context, cwd, text string) (Result, error) {
	report, err := t.apply(ctx, cwd, text)
	data, _ := json.Marshal(report)
	result := Result{Content: []model.Block{{Type: "text", Text: string(data)}}}
	if err == nil {
		return result, nil
	}
	var failure *patch.Error
	if errors.As(err, &failure) {
		return result, protocol.E(failure.Code, failure.Error())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, protocol.E("patch_cancelled", "patch cancelled; inspect file results before retrying")
	}
	return result, protocol.E("patch_io", "patch failed; inspect file results before retrying")
}
