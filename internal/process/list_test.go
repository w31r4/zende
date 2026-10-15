package process

import (
	"context"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func TestProcessListScopesPaginatesAndPreservesIdentity(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	for _, spec := range []Spec{{ID: "b", AgentID: "agent", TaskID: "tb", Cwd: t.TempDir(), Command: "true"}, {ID: "a", AgentID: "agent", TaskID: "ta", Cwd: t.TempDir(), Command: "true"}, {ID: "other", AgentID: "other", Cwd: t.TempDir(), Command: "true"}} {
		if _, err := m.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Wait(ctx, spec.ID, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	first, err := m.List(ctx, ListParams{AgentID: "agent", Limit: 1})
	if err != nil || len(first.Processes) != 1 || first.Processes[0].ID != "a" || first.Processes[0].TaskID != "ta" || first.Processes[0].Status != "exited" || first.Next != "a" {
		t.Fatal(first, err)
	}
	second, err := m.List(ctx, ListParams{AgentID: "agent", After: first.Next, Limit: 1})
	if err != nil || len(second.Processes) != 1 || second.Processes[0].ID != "b" || second.Next != "" {
		t.Fatal(second, err)
	}
	second.Processes[0].Output["stdout"] = StreamState{Total: 100}
	state, _ := m.Inspect(ctx, "b")
	if state.Output["stdout"].Total != 0 {
		t.Fatal("list aliased owned state")
	}
	all, err := m.List(ctx, ListParams{})
	if err != nil || len(all.Processes) != 3 {
		t.Fatal(all, err)
	}
	for _, p := range []ListParams{{Limit: 101}, {Limit: -1}, {After: "../bad"}} {
		if _, err := m.List(ctx, p); err == nil {
			t.Fatal("invalid list accepted")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.List(cancelled, ListParams{}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestProcessListFrameBudget(t *testing.T) {
	m := manager(t, 4096)
	for _, id := range []string{"a", "b"} {
		m.entries[id] = &entry{state: State{Spec: Spec{ID: id, Command: strings.Repeat("x", protocol.MaxFrameBytes/2)}, Status: "exited"}}
	}
	page, err := m.List(context.Background(), ListParams{})
	if err != nil || len(page.Processes) != 1 || page.Next != "a" {
		t.Fatal(len(page.Processes), page.Next, err)
	}
	m.entries["a"].state.Command = strings.Repeat("x", protocol.MaxFrameBytes)
	if _, err := m.List(context.Background(), ListParams{}); err == nil {
		t.Fatal("oversized record accepted")
	}
}
