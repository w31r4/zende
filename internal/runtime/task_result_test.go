package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

func TestTaskWaitLoopDoesNotWaitForParentMailboxOrRepeatDelivery(t *testing.T) {
	var c *Core
	var parent protocol.Accepted
	var child protocol.Accepted
	var parentRequests, childRequests, executions atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		events := fixture.Answer("final", "child-evidence")
		if strings.Contains(r.System, "\nAgent: "+parent.AgentID+"\n") {
			n := parentRequests.Add(1)
			if n == 1 {
				events = fixture.Call("wait-call", "exec_command", "{}")
			} else if n == 2 {
				text, _ := json.Marshal(r.Messages)
				if strings.Count(string(text), "child-evidence") != 1 || !strings.Contains(string(text), "peer output") {
					return fmt.Errorf("missing or duplicate wait body")
				}
				events = fixture.Answer("final", "parent done")
			} else {
				return fmt.Errorf("extra parent processing: %d", n)
			}
		} else {
			childRequests.Add(1)
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c = loopCore(t, adapter, testTool{"exec_command", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		executions.Add(1)
		caller := &protocol.Caller{AgentID: call.AgentID, TaskID: call.TaskID, OperationID: call.ID}
		p := protocol.SpawnAgent{ParentID: call.AgentID, Input: "child-work", Context: "none", Caller: caller}
		req, _ := protocol.NewRequest("agent.spawn", "child", p)
		var err error
		child, err = c.spawnAgent(ctx, req, p)
		if err != nil {
			return tool.Result{}, err
		}
		out, err := c.waitTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, WaitMS: 3000, Caller: caller})
		if err != nil || !out.Terminal || out.State != "completed" {
			return tool.Result{}, fmt.Errorf("wait blocked on parent: %+v %v", out, err)
		}
		// The real shell may redirect stdout; delivery is associated with process
		// identity, not the bytes printed by that shell.
		return tool.Result{ProcessID: call.ID, Content: []model.Block{{Type: "text", Text: "stdout redirected"}}}, nil
	}})
	parent = createWork(t, c, "parent-work")
	runCore(t, c)
	snap := waitTask(t, c, parent, "completed")
	page, err := c.Store.Mailbox(context.Background(), protocol.MailboxQuery{AgentID: parent.AgentID})
	if err != nil || len(page.Items) != 1 || page.Items[0].TaskID != parent.TaskID || page.Items[0].State != "processed" {
		t.Fatal(page, err)
	}
	if len(snap.Tasks) != 1 || executions.Load() != 1 || parentRequests.Load() != 2 || childRequests.Load() != 1 {
		t.Fatal(snap, executions.Load(), parentRequests.Load(), childRequests.Load())
	}
}

func TestTaskWaitTimeoutCancellationFailureAndLargeResult(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	parent := createWork(t, c, "parent")
	if _, err := c.Store.ClaimNext(ctx, parent.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := &protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "original-process"}
	if err := c.Store.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", invocation{ID: caller.OperationID, RequestID: "request", Call: model.ToolCall{ID: "call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	spawn := func() protocol.Accepted {
		t.Helper()
		p := protocol.SpawnAgent{ParentID: parent.AgentID, Context: "none", Input: "child"}
		req, _ := protocol.NewRequest("agent.spawn", protocol.NewID("spawn"), p)
		child, err := c.spawnAgent(ctx, req, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Store.ClaimNext(ctx, child.AgentID); err != nil {
			t.Fatal(err)
		}
		return child
	}
	child := spawn()
	p := protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller}
	out, err := c.waitTask(ctx, p)
	if err != nil || out.Terminal || !out.TimedOut || out.State != "running" {
		t.Fatal(out, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.waitTask(cancelled, p); err == nil {
		t.Fatal("cancelled observation succeeded")
	}
	snap, _ := c.Store.Snapshot(ctx, child.AgentID)
	if snap.Tasks[0].State != "running" {
		t.Fatal("wait cancelled child", snap)
	}
	for _, n := range []int{-1, 30001} {
		bad := p
		bad.WaitMS = n
		if _, err := c.waitTask(ctx, bad); err == nil {
			t.Fatal("bad wait accepted")
		}
	}
	body := strings.Repeat("x", protocol.MaxTaskResultBytes)
	if err := c.Store.FinishTask(ctx, child.AgentID, child.TaskID, "completed", body, nil); err != nil {
		t.Fatal(err)
	}
	out, err = c.waitTask(ctx, p)
	if err != nil || out.Result == nil || out.Bytes != len(body) {
		t.Fatal(out, err)
	}
	result := tool.Result{ProcessID: caller.OperationID, Content: []model.Block{{Type: "text", Text: "reference only"}}}
	got, delivery, err := c.attachWaitResults(ctx, parent.AgentID, model.ToolCall{Name: "write_stdin"}, result)
	if err != nil || len(delivery.Messages) != 0 || len(got.Content) != 1 {
		t.Fatal(delivery, err)
	}
	mail, _ := c.Store.Mail(ctx, out.MessageID)
	if mail.DeliveredSeq != 0 {
		t.Fatal("oversized result consumed", mail.MailInfo)
	}
	if err := c.Store.FinishTask(ctx, parent.AgentID, parent.TaskID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := c.Store.ClaimNext(ctx, parent.AgentID)
	if err != nil || queued == nil {
		t.Fatal(queued, err)
	}
	history, _, err := c.Store.ModelContext(ctx, parent.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	last := history[len(history)-1]
	if len(last.Content) != 2 || last.Content[1].Text != body || len(last.Content[0].Text) > 1024 {
		t.Fatal("full result not retained in bounded blocks")
	}
	if err := c.Store.FinishTask(ctx, parent.AgentID, queued.Task.ID, "completed", "read", nil); err != nil {
		t.Fatal(err)
	}
	failed := spawn()
	if err := c.Store.FinishTask(ctx, failed.AgentID, failed.TaskID, "failed", "", protocol.E("fixture", "known failure")); err != nil {
		t.Fatal(err)
	}
	out, err = c.waitTask(ctx, protocol.WaitTask{AgentID: failed.AgentID, TaskID: failed.TaskID})
	if err != nil || !out.Terminal || out.State != "failed" || out.Result != nil || out.Error == nil || out.Error.Data.Kind != "fixture" {
		t.Fatal(out, err)
	}
}

func TestTaskWaitWriteStdinUsesOriginalProcessAndRecipient(t *testing.T) {
	ctx := context.Background()
	c := loopCore(t, nil)
	parent := createWork(t, c, "parent")
	c.Store.ClaimNext(ctx, parent.AgentID)
	caller := &protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "original-process"}
	if err := c.Store.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", invocation{ID: caller.OperationID, RequestID: "request", Call: model.ToolCall{ID: "call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Context: "none", Input: "child"}
	req, _ := protocol.NewRequest("agent.spawn", "child", p)
	child, err := c.spawnAgent(ctx, req, p)
	if err != nil {
		t.Fatal(err)
	}
	c.Store.ClaimNext(ctx, child.AgentID)
	if err := c.Store.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "precise body", nil); err != nil {
		t.Fatal(err)
	}
	out, err := c.waitTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{child.AgentID, parent.AgentID} {
		result, delivery, err := c.attachWaitResults(ctx, agent, model.ToolCall{ID: "new-poll-call", Name: "write_stdin"}, tool.Result{ProcessID: caller.OperationID})
		if err != nil {
			t.Fatal(err)
		}
		if agent == child.AgentID {
			if len(delivery.Messages) != 0 {
				t.Fatal("wrong recipient")
			}
			continue
		}
		if len(delivery.Messages) != 1 || delivery.Messages[0] != out.MessageID {
			t.Fatal(delivery)
		}
		message, err := toolMessage(model.ToolCall{ID: "new-poll-call"}, result, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Store.RecordToolResult(ctx, parent.AgentID, parent.TaskID, "request", nil, message, delivery); err != nil {
			t.Fatal(err)
		}
	}
	// Re-observing an already delivered result cannot create another tool block.
	_, delivery, err := c.attachWaitResults(ctx, parent.AgentID, model.ToolCall{Name: "write_stdin"}, tool.Result{ProcessID: caller.OperationID})
	if err != nil || len(delivery.Messages) != 0 {
		t.Fatal(delivery, err)
	}
}

func TestTaskWaitHonorsResumeBeforeOldToolSettles(t *testing.T) {
	ctx := context.Background()
	var c *Core
	var parent protocol.Accepted
	observed := make(chan protocol.TaskOutcome, 1)
	release := make(chan struct{}, 1)
	defer close(release)
	var parents atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		events := fixture.Answer("final", "resume-child-evidence")
		if strings.Contains(r.System, "\nAgent: "+parent.AgentID+"\n") {
			n := parents.Add(1)
			if n == 1 {
				events = fixture.Call("wait-call", "exec_command", "{}")
			} else {
				raw, _ := json.Marshal(r.Messages)
				if n != 2 || strings.Count(string(raw), "resume-child-evidence") != 1 {
					return fmt.Errorf("resume lost or repeated context")
				}
				for _, message := range r.Messages {
					if len(message.Content) == 0 {
						return fmt.Errorf("empty resume input")
					}
				}
				events = fixture.Answer("final", "resumed")
			}
		}
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c = loopCore(t, adapter, testTool{"exec_command", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		caller := &protocol.Caller{AgentID: call.AgentID, TaskID: call.TaskID, OperationID: call.ID}
		p := protocol.SpawnAgent{ParentID: call.AgentID, Input: "child", Context: "none", Caller: caller}
		req, _ := protocol.NewRequest("agent.spawn", "child", p)
		child, err := c.spawnAgent(ctx, req, p)
		if err != nil {
			return tool.Result{}, err
		}
		out, err := c.waitTask(ctx, protocol.WaitTask{AgentID: child.AgentID, TaskID: child.TaskID, Caller: caller, WaitMS: 3000})
		if err != nil || !out.Terminal {
			return tool.Result{}, fmt.Errorf("wait: %+v %v", out, err)
		}
		observed <- out
		// An already-started tool is allowed to settle after stop and resume.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return tool.Result{}, fmt.Errorf("fixture release timeout")
		}
		return tool.Result{ProcessID: call.ID, Content: []model.Block{{Type: "text", Text: "observed"}}}, nil
	}})
	parent = createWork(t, c, "parent")
	runCore(t, c)
	var outcome protocol.TaskOutcome
	select {
	case outcome = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("wait not observed")
	}
	control := func(action string) protocol.Accepted {
		t.Helper()
		req, _ := protocol.NewRequest("agent."+action, action, protocol.Target{AgentID: parent.AgentID})
		value, err := c.Handle(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return value.(protocol.Accepted)
	}
	control("stop")
	resumed := control("resume")
	// Send one release value; the deferred close also releases failure paths.
	release <- struct{}{}
	waitTask(t, c, parent, "cancelled")
	snap := waitTask(t, c, resumed, "completed")
	if len(snap.Tasks) != 2 || parents.Load() != 2 {
		t.Fatal(snap, parents.Load())
	}
	mail, err := c.Store.Mail(ctx, outcome.MessageID)
	if err != nil || mail.TaskID != parent.TaskID || mail.DeliveredSeq == 0 {
		t.Fatal(mail, err)
	}
	page, err := c.Store.Events(ctx, protocol.Cursor{AgentID: parent.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for _, event := range page.Events {
		if event.Kind == "mailbox.delivered" {
			deliveries++
		}
	}
	if deliveries != 1 {
		t.Fatal("duplicate delivery", deliveries)
	}
}
