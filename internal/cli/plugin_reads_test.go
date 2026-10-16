package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/store"
)

type pluginCLIReadCase struct {
	receipt store.PluginReceipt
	result  []byte
}

// The test owner invokes a real fixture and retains its journal. This tests
// read adapters, not the still-unimplemented public plugin execution entry.
func seedPluginCLIReads(t *testing.T, ctx context.Context, s *store.Store, source string, publication protocol.Accepted, run func(bool, ...string) []byte) []pluginCLIReadCase {
	t.Helper()
	var cases []pluginCLIReadCase
	for _, tc := range []struct{ mode, kind, state string }{{"", "tool", "completed"}, {"call-failed", "command", "failed"}, {"call-cancel", "command", "cancelled"}, {"call-exit", "tool", "unknown"}} {
		t.Setenv("ZENDE_TEST_PLUGIN_MODE", tc.mode)
		pkg, instance, err := resource.PreparePluginPackage(ctx, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { instance.Close() })
		if pkg.Info.ID != publication.Resource.ID || pkg.Info.Version != publication.Resource.Version {
			t.Fatal("fixture differs from publication")
		}
		var ref string
		for _, d := range pkg.Plugin.Definitions {
			if d.Kind == tc.kind {
				ref = d.Ref
			}
		}
		op := "cli-read-" + tc.state
		args, _ := json.Marshal(map[string]string{"text": "private-value\x1b[31m" + strings.Repeat("中文结果", 1024)})
		call, err := instance.PrepareCall(ref, op, plugin.Scope{AgentID: publication.AgentID, Cwd: filepath.Dir(source)}, args)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := protocol.NewRequest("plugin."+tc.kind+".call", op, store.PluginCallInput{AgentID: publication.AgentID, ResourceID: pkg.Info.ID, DefinitionRef: ref, Arguments: args})
		inv := store.PluginInvocation{Request: req, Binding: call.Binding(), Scope: call.Scope(), Definition: call.Definition(), ResourceVersion: pkg.Info.Version, Generation: publication.ResourceGeneration}
		if _, fresh, err := s.BeginPluginCall(ctx, inv, "cli-fixture"); err != nil || !fresh {
			t.Fatal(fresh, err)
		}
		target := protocol.PluginCallTarget{AgentID: publication.AgentID, OperationID: op}
		var pending store.PluginReceipt
		if err := json.Unmarshal(run(true, "plugin", "inspect", target.AgentID, op), &pending); err != nil || pending.State != "dispatched" || pending.ResultBytes != 0 {
			t.Fatal(pending, err)
		}
		var failure protocol.Error
		if err := json.Unmarshal(run(false, "plugin", "result", target.AgentID, op), &failure); err != nil || failure.Data.Kind != "unavailable" {
			t.Fatal(failure, err)
		}
		callCtx, cancel := context.WithCancel(ctx)
		result, invokeErr := call.Invoke(callCtx, func(p plugin.Progress) error {
			if err := s.AppendPluginProgress(ctx, target, "cli-fixture", p); err != nil {
				return err
			}
			if tc.mode == "call-cancel" && p.Sequence == 1 {
				record, err := s.MarkPluginCancelling(ctx, target, "cli-fixture")
				if err != nil {
					return err
				}
				if record.Receipt.State != "cancelling" {
					return protocol.E("invalid_state", "cancellation fabricated a terminal result")
				}
				cancel()
			}
			return nil
		})
		cancel()
		if result.State != tc.state || (invokeErr != nil) != (tc.state == "unknown") {
			t.Fatal(result, invokeErr)
		}
		record, err := s.FinishPluginCall(ctx, target, "cli-fixture", result)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, pluginCLIReadCase{record.Receipt, raw})
		instance.Close()
	}
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
	return cases
}

func checkPluginCLIReads(t *testing.T, cases []pluginCLIReadCase, run func(bool, ...string) []byte) {
	t.Helper()
	for _, tc := range cases {
		agent, op := tc.receipt.AgentID, tc.receipt.OperationID
		var receipt store.PluginReceipt
		raw := run(true, "plugin", "inspect", agent, op)
		if err := json.Unmarshal(raw, &receipt); err != nil || receipt.State != tc.receipt.State || receipt.Binding != tc.receipt.Binding || receipt.ResultSHA256 != tc.receipt.ResultSHA256 || bytes.Contains(raw, []byte("private-value")) || bytes.Contains(raw, []byte("private-failure")) || bytes.Contains(raw, []byte("arguments")) {
			t.Fatal(receipt, err)
		}
		var after int64
		for pages := 0; ; pages++ {
			var page store.PluginProgressPage
			if err := json.Unmarshal(run(true, "plugin", "progress", agent, op, "--after", strconv.FormatInt(after, 10), "--limit", "1"), &page); err != nil || page.AgentID != agent || page.OperationID != op || len(page.Items) != 1 || page.Next != after+1 || page.Items[0].Sequence != page.Next || page.Items[0].RequestID != receipt.RequestID {
				t.Fatal(page, err)
			}
			after = page.Next
			if !page.More {
				break
			}
			if pages > 2 {
				t.Fatal("unexpected progress count")
			}
		}
		if after != receipt.ProgressSeq {
			t.Fatal("missing progress", after, receipt)
		}
		var tail store.PluginProgressPage
		if err := json.Unmarshal(run(true, "plugin", "progress", agent, op, "--after", strconv.FormatInt(after, 10)), &tail); err != nil || len(tail.Items) != 0 || tail.More || tail.Next != after {
			t.Fatal(tail, err)
		}
		var assembled []byte
		var offset int
		for pages := 0; ; pages++ {
			var page store.PluginResultPage
			if err := json.Unmarshal(run(true, "plugin", "result", agent, op, "--offset", strconv.Itoa(offset), "--limit", "4096"), &page); err != nil || page.AgentID != agent || page.OperationID != op || page.Offset != offset || page.Next != offset+len(page.Text) || !utf8.ValidString(page.Text) || page.SHA256 != receipt.ResultSHA256 || page.Total != receipt.ResultBytes {
				t.Fatal(page, err)
			}
			assembled = append(assembled, page.Text...)
			offset = page.Next
			if !page.More {
				break
			}
			if len(page.Text) == 0 || pages > 32 {
				t.Fatal("result pagination did not advance")
			}
		}
		digest := sha256.Sum256(assembled)
		if !bytes.Equal(assembled, tc.result) || hex.EncodeToString(digest[:]) != receipt.ResultSHA256 {
			t.Fatal("result bytes changed")
		}
		if middle := bytes.Index(assembled, []byte("中")); middle >= 0 {
			var failure protocol.Error
			if err := json.Unmarshal(run(false, "plugin", "result", agent, op, "--offset", strconv.Itoa(middle+1)), &failure); err != nil || failure.Data.Kind != "invalid_params" {
				t.Fatal("accepted offset inside UTF-8 character", failure, err)
			}
		}
		for _, invalid := range []struct {
			args []string
			kind string
		}{
			{[]string{"inspect", "another-agent", op}, "not_found"},
			{[]string{"progress", "another-agent", op}, "not_found"},
			{[]string{"result", "another-agent", op}, "not_found"},
			{[]string{"progress", agent, op, "--after", "999"}, "cursor_gap"},
			{[]string{"progress", agent, op, "--limit", "33"}, "invalid_params"},
			{[]string{"result", agent, op, "--limit", "3"}, "invalid_params"},
			{[]string{"result", agent, op, "--offset", "-1"}, "invalid_params"},
		} {
			var failure protocol.Error
			if err := json.Unmarshal(run(false, append([]string{"plugin"}, invalid.args...)...), &failure); err != nil || failure.Data.Kind != invalid.kind {
				t.Fatal(invalid.args, failure, err)
			}
		}
	}
}
