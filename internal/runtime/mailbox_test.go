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

func runtimeMail(t *testing.T, c *Core, agent, mode, text string) protocol.Accepted {
	t.Helper()
	p := protocol.SendMessage{AgentID: agent, Delivery: mode, Input: text}
	req, _ := protocol.NewRequest("agent.send", protocol.NewID("mail"), p)
	value, err := c.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.Accepted)
}
func waitMail(t *testing.T, c *Core, a protocol.Accepted, state string) protocol.Mail {
	t.Helper()
	var m protocol.Mail
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var err error
		m, err = c.Store.Mail(context.Background(), a.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		if m.State == state {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mail not", state, m)
	return m
}

func TestMailboxQueueSteerWaitForWholeToolGroupAndKeepOneLoop(t *testing.T) {
	ctx := context.Background()
	started := make(chan int, 2)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var executions atomic.Int32
	var requests atomic.Int32
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		n := requests.Add(1)
		raw, _ := json.Marshal(r.Messages)
		switch n {
		case 1:
			events := fixture.Call("first", "gate", "{}")
			events = append(events[:len(events)-1], fixture.Call("second", "gate", "{}")...)
			for _, event := range events {
				if err := emit(event); err != nil {
					return err
				}
			}
			return nil
		case 2:
			if strings.Contains(string(raw), "queued-evidence") || !strings.Contains(string(raw), "steer-evidence") {
				return fmt.Errorf("wrong delivery at safe boundary")
			}
			results := 0
			for _, m := range r.Messages {
				if m.Role == "tool" {
					results++
				}
				if len(m.Content) > 0 && strings.HasPrefix(m.Content[0].Text, "Mailbox message.") && results != 2 {
					return fmt.Errorf("mail split tool results")
				}
			}
			if results != 2 {
				return fmt.Errorf("incomplete tool group: %d", results)
			}
		case 3:
			if !strings.Contains(string(raw), "queued-evidence") {
				return fmt.Errorf("queue missing after completion")
			}
		default:
			return fmt.Errorf("duplicate processing request")
		}
		for _, event := range fixture.Answer("final", "done") {
			if err := emit(event); err != nil {
				return err
			}
		}
		return nil
	})
	c := loopCore(t, adapter, testTool{"gate", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		n := int(executions.Add(1)) - 1
		if n > 1 {
			return tool.Result{}, fmt.Errorf("tool replayed")
		}
		started <- n
		select {
		case <-ctx.Done():
			return tool.Result{}, fmt.Errorf("steer cancelled original tool: %w", ctx.Err())
		case <-release[n]:
			return tool.Result{Content: []model.Block{{Type: "text", Text: "tool completed"}}}, nil
		}
	}})
	a := createWork(t, c, "original")
	runCore(t, c)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first tool did not start")
	}
	q := runtimeMail(t, c, a.AgentID, "queue", "queued-evidence")
	st := runtimeMail(t, c, a.AgentID, "steer", "steer-evidence")
	for _, m := range []protocol.Accepted{q, st} {
		if saved, _ := c.Store.Mail(ctx, m.MessageID); saved.State != "accepted" {
			t.Fatal(saved)
		}
	}
	close(release[0])
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second tool did not start")
	}
	if saved, _ := c.Store.Mail(ctx, st.MessageID); saved.State != "accepted" {
		t.Fatal("delivered between calls", saved)
	}
	close(release[1])
	waitTask(t, c, a, "completed")
	steered := waitMail(t, c, st, "processed")
	queued := waitMail(t, c, q, "processed")
	if steered.TaskID != a.TaskID || queued.TaskID == a.TaskID || requests.Load() != 3 || executions.Load() != 2 {
		t.Fatal(steered, queued, requests.Load(), executions.Load())
	}
	snap, err := c.Store.Snapshot(ctx, a.AgentID)
	if err != nil || len(snap.Tasks) != 2 {
		t.Fatal(snap, err)
	}
}

func TestMailboxPausedAndIdleSchedulingBothModes(t *testing.T) {
	for _, mode := range []string{"queue", "steer"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int32
			adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
				count.Add(1)
				for _, event := range fixture.Answer("final", "received") {
					if err := emit(event); err != nil {
						return err
					}
				}
				return nil
			})
			c := loopCore(t, adapter)
			a := createWork(t, c, "")
			req, _ := protocol.NewRequest("agent.stop", "pause", protocol.Target{AgentID: a.AgentID})
			if _, err := c.Handle(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			m := runtimeMail(t, c, a.AgentID, mode, "while paused")
			runCore(t, c)
			time.Sleep(100 * time.Millisecond)
			snap, _ := c.Store.Snapshot(context.Background(), a.AgentID)
			if count.Load() != 0 || len(snap.Tasks) != 0 || !snap.Agent.Paused {
				t.Fatal(snap, count.Load())
			}
			req, _ = protocol.NewRequest("agent.resume", "resume", protocol.Target{AgentID: a.AgentID})
			if _, err := c.Handle(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			waitMail(t, c, m, "processed")
			idle := runtimeMail(t, c, a.AgentID, mode, "after completion")
			waitMail(t, c, idle, "processed")
			if count.Load() != 2 {
				t.Fatal("lost or duplicated mailbox", count.Load())
			}
		})
	}
}
