package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/patch"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

func TestLoopPatchConflictRecoveryAndShellGenerator(t *testing.T) {
	_, remote, _ := remoteRunner(t)
	var requests, dispatches atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		var events []model.Delta
		callPatch := func(body string) []model.Delta {
			args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n" + body + "*** End Patch\n"})
			return fixture.Call(fmt.Sprint("patch-", requests.Load()), "apply_patch", string(args))
		}
		switch requests.Add(1) {
		case 1:
			if len(r.Tools) != 3 || !strings.Contains(r.System, "apply_patch") {
				return fmt.Errorf("patch missing from registered capabilities")
			}
			events = callPatch("*** Add File: edited\n+old\n")
		case 2:
			var report patch.Report
			last := r.Messages[len(r.Messages)-1]
			if last.Content[0].IsError || json.Unmarshal([]byte(last.Content[0].Text), &report) != nil || report.Status != "applied" {
				return fmt.Errorf("patch result missing")
			}
			events = callPatch("*** Add File: must-not-exist\n+first\n*** Update File: edited\n@@\n-stale\n+wrong\n")
		case 3:
			var report patch.Report
			last := r.Messages[len(r.Messages)-1]
			if !last.Content[0].IsError || json.Unmarshal([]byte(last.Content[0].Text), &report) != nil || report.Status != "rejected" {
				return fmt.Errorf("conflict not returned to model")
			}
			events = callPatch("*** Update File: edited\n@@\n-old\n+new\n")
		case 4:
			args, _ := json.Marshal(map[string]any{"command": "test ! -e must-not-exist && test \"$(cat edited)\" = new && printf generated > generated", "wait_ms": 3000})
			events = fixture.Call("generator", "exec_command", string(args))
		case 5:
			var output tool.ProcessResult
			last := r.Messages[len(r.Messages)-1]
			if last.Content[0].IsError || json.Unmarshal([]byte(last.Content[0].Text), &output) != nil || output.ExitCode == nil || *output.ExitCode != 0 {
				return fmt.Errorf("shell generator failed")
			}
			events = fixture.Answer("final", "patch and generator completed")
		default:
			return fmt.Errorf("unexpected request")
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c := loopCore(t, adapter)
	var executors []tool.Executor
	for _, executor := range append(tool.ProcessExecutors(remote), tool.PatchExecutor()) {
		executors = append(executors, checkedIntent{executor, c.Store, &dispatches})
	}
	var err error
	c.Tools, err = tool.NewRegistry(executors...)
	if err != nil {
		t.Fatal(err)
	}
	a := createWork(t, c, "edit and generate")
	runCore(t, c)
	waitTask(t, c, a, "completed")
	if requests.Load() != 5 || dispatches.Load() != 4 {
		t.Fatal(requests.Load(), dispatches.Load())
	}
	for name, want := range map[string]string{"edited": "new\n", "generated": "generated"} {
		data, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, name))
		if err != nil || string(data) != want {
			t.Fatal(name, string(data), err)
		}
	}
	page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, event := range page.Events {
		if event.Kind == "tool.completed" {
			completed++
		}
	}
	if completed != 4 {
		t.Fatalf("durable tool results %d", completed)
	}
}

func TestIncompletePatchCallNeverWrites(t *testing.T) {
	script := fixture.New(fixture.Step{Events: []model.Delta{
		{Kind: model.CallStart, CallID: "partial", Name: "apply_patch"},
		{Kind: model.ArgumentsDelta, CallID: "partial", Text: `{"patch":"*** Begin Patch\n*** Add File: forbidden\n+bad\n`},
	}})
	c := loopCore(t, script, tool.PatchExecutor())
	a := createWork(t, c, "partial patch")
	runCore(t, c)
	waitTask(t, c, a, "failed")
	if _, err := os.Lstat(filepath.Join(c.Config.Config.Cwd, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("incomplete patch wrote", err)
	}
}
