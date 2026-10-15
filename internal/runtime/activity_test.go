package runtime

import (
	"context"
	"testing"

	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

type activityRunner struct {
	pages []process.ListPage
	calls int
	err   error
}

func (*activityRunner) Start(context.Context, process.Spec) (process.State, error) {
	panic("activity must not execute")
}
func (*activityRunner) Inspect(context.Context, string) (process.State, error) {
	panic("activity must not inspect unrelated individual processes")
}
func (r *activityRunner) List(_ context.Context, p process.ListParams) (process.ListPage, error) {
	if p.Limit != 100 {
		panic("unbounded page")
	}
	r.calls++
	if r.err != nil {
		return process.ListPage{}, r.err
	}
	return r.pages[min(r.calls-1, len(r.pages)-1)], nil
}

func TestActivityObservesProcessesWithoutControllingThem(t *testing.T) {
	c, client, cancel, done := serveTest(t)
	defer func() { cancel(); <-done }()
	ctx := context.Background()
	p := protocol.CreateAgent{Cwd: t.TempDir(), Input: "work"}
	req, _ := protocol.NewRequest("agent.create", "work", p)
	a, err := c.Store.Create(ctx, req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	runner := &activityRunner{pages: []process.ListPage{
		{Processes: []process.State{{Spec: process.Spec{ID: "a", AgentID: a.AgentID, PTY: true}, Status: "running"}, {Spec: process.Spec{ID: "b", AgentID: "unrelated"}, Status: "running"}}, Next: "b"},
		{Processes: []process.State{{Spec: process.Spec{ID: "c", AgentID: a.AgentID}, Status: "unknown"}, {Spec: process.Spec{ID: "d", AgentID: a.AgentID}, Status: "exited"}}},
	}}
	c.Runner = runner
	req, _ = protocol.NewRequest("agent.activity", "", protocol.Target{AgentID: a.AgentID})
	var result protocol.Activity
	if err := client.Call(ctx, req, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 3 || len(result.Unavailable) != 0 || runner.calls != 2 || !protocol.ReadOnly("agent.activity") {
		t.Fatal(result, runner.calls)
	}
	if result.Items[1].Kind != "tmux" || result.Items[2].Kind != "pipe" {
		t.Fatal(result)
	}
	runner.err = protocol.E("unavailable", "offline")
	if err := client.Call(ctx, req, &result); err != nil || len(result.Items) != 1 || len(result.Unavailable) != 1 {
		t.Fatal(result, err)
	}
}
