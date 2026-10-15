package runtime

import (
	"bytes"
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

func TestQuestionLoopWaitsAfterToolsAndReceivesTextAndImages(t *testing.T) {
	ctx := context.Background()
	asked := make(chan protocol.Accepted, 1)
	var c *Core
	var calls atomic.Int32
	script := fixture.New(fixture.Step{Events: fixture.Call("ask-call", "exec_command", `{}`)}, fixture.Step{Events: fixture.Answer("final", "answered"), Check: func(req model.Request) error {
		if len(req.Messages) != 4 || req.Messages[2].Role != "tool" || req.Messages[3].Role != "user" {
			return fmt.Errorf("wrong answer ordering: %+v", req.Messages)
		}
		content := req.Messages[3].Content
		if len(content) != 2 || !strings.Contains(content[0].Text, "中文\n回答") || content[1].Type != "image" || len(req.Images[content[1].Attachment].Data) == 0 {
			return fmt.Errorf("answer or image lost")
		}
		if !strings.Contains(req.System, "Task: ") || !strings.Contains(req.System, "question ask") {
			return fmt.Errorf("missing registered CLI or task identity")
		}
		return nil
	}})
	adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
		if r.Messages[0].Content[0].Text == "independent" {
			for _, d := range fixture.Answer("final", "other finished") {
				if err := emit(d); err != nil {
					return err
				}
			}
			return nil
		}
		calls.Add(1)
		return script.Stream(ctx, r, emit)
	})
	c = loopCore(t, adapter, testTool{name: "exec_command", execute: func(ctx context.Context, call tool.Call) (tool.Result, error) {
		p := protocol.AskQuestion{AgentID: call.AgentID, TaskID: call.TaskID, Prompt: "看一下图片？"}
		r, _ := protocol.NewRequest("question.ask", call.ID+"-ask", p)
		result, err := c.Handle(ctx, r)
		if err != nil {
			return tool.Result{}, err
		}
		a := result.(protocol.Accepted)
		asked <- a
		raw, _ := json.Marshal(a)
		return tool.Result{Content: []model.Block{{Type: "text", Text: string(raw)}}}, nil
	}})
	c.Commands = []CommandHint{{Name: "question.ask", Usage: "zende question ask AGENT --task TASK --file FILE", Guidance: "Ask for missing user information."}}
	c.Models = model.ResolveFunc(func(context.Context, string) (model.Binding, error) {
		return model.Binding{Adapter: adapter, Model: "fixture", Protocol: "fixture", Images: true, NativeCategories: true, MaxOutputTokens: 4096}, nil
	})
	a := createWork(t, c, "ask user")
	runCore(t, c)
	var question protocol.Accepted
	select {
	case question = <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("question not asked")
	}
	// A different Agent can finish while the original worker waits for input.
	other := createWork(t, c, "independent")
	waitTask(t, c, other, "completed")
	if calls.Load() != 1 {
		t.Fatal("model requested again without an answer", calls.Load())
	}
	snap, err := c.Store.Snapshot(ctx, a.AgentID)
	if err != nil || snap.Tasks[0].State != "running" || len(snap.Questions) != 1 {
		t.Fatal(snap, err)
	}
	path, data := inputImageFile(t, c)
	_, image := importTestImage(t, c, a.AgentID, path)
	p := protocol.AnswerQuestion{AgentID: a.AgentID, ID: question.QuestionID, Answer: "中文\n回答", Attachments: []string{image.Attachment.ID}}
	r, _ := protocol.NewRequest("question.answer", "answer", p)
	if _, err := c.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	waitTask(t, c, a, "completed")
	if calls.Load() != 2 {
		t.Fatal("unexpected model requests", calls.Load())
	}
	q, err := c.Store.Question(ctx, protocol.QuestionTarget{AgentID: a.AgentID, ID: question.QuestionID})
	if err != nil || q.State != "answered" || q.DeliveredSeq == 0 {
		t.Fatal(q, err)
	}
	imageData, err := c.Store.ReadImage(ctx, protocol.AttachmentTarget{AgentID: a.AgentID, ID: image.Attachment.ID})
	if err != nil || !bytes.Equal(data, imageData.Data) {
		t.Fatal("image changed", err)
	}
}

func TestQuestionWaitingSurvivesRecoveryWithoutRepeatingTool(t *testing.T) {
	for _, earlyAnswer := range []bool{false, true} {
		t.Run(fmt.Sprint(earlyAnswer), func(t *testing.T) {
			ctx := context.Background()
			script := fixture.New(fixture.Step{Events: fixture.Answer("final", "recovered answer"), Check: func(req model.Request) error {
				if req.System != "original system prompt" || len(req.Messages) != 4 || req.Messages[2].Role != "tool" || req.Messages[3].Role != "user" || !strings.Contains(req.Messages[3].Content[0].Text, "retained answer") {
					return fmt.Errorf("recovery lost answer or tool ordering")
				}
				return nil
			}})
			c := loopCore(t, script, testTool{name: "ask_fixture", execute: func(context.Context, tool.Call) (tool.Result, error) {
				return tool.Result{}, fmt.Errorf("completed question tool was replayed")
			}})
			a, appendEvent, _ := savedWork(t, c, script)
			call := model.ToolCall{ID: "original-call", Name: "ask_fixture", Arguments: json.RawMessage(`{}`)}
			journalResponse(t, c, a, call)
			op := invocation{SchemaVersion: 2, ID: "question-operation", RequestID: "request-original", Call: call, Version: "fixture-1"}
			appendEvent("tool.started", op)
			appendEvent("tool.dispatching", op)
			ask := protocol.AskQuestion{AgentID: a.AgentID, TaskID: a.TaskID, Prompt: "Continue?"}
			askReq, _ := protocol.NewRequest("question.ask", "question-ask", ask)
			result, err := c.Handle(ctx, askReq)
			if err != nil {
				t.Fatal(err)
			}
			q := result.(protocol.Accepted)
			outcome := tool.Result{Content: []model.Block{{Type: "text", Text: q.QuestionID}}}
			message, err := toolMessage(call, outcome, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Store.RecordToolResult(ctx, a.AgentID, a.TaskID, op.RequestID, toolOutcome{op, outcome, nil}, message); err != nil {
				t.Fatal(err)
			}
			answer := protocol.AnswerQuestion{AgentID: a.AgentID, ID: q.QuestionID, Answer: "retained answer"}
			ar, _ := protocol.NewRequest("question.answer", "question-answer", answer)
			if earlyAnswer {
				if _, err := c.Handle(ctx, ar); err != nil {
					t.Fatal(err)
				}
			}
			c = reopenCore(t, c)
			runCore(t, c)
			if !earlyAnswer {
				for deadline := time.Now().Add(5 * time.Second); ; {
					snap, err := c.Store.Snapshot(ctx, a.AgentID)
					if err != nil {
						t.Fatal(err)
					}
					if snap.Tasks[0].State == "running" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("recovery did not reach question wait", snap)
					}
					time.Sleep(5 * time.Millisecond)
				}
				if len(script.Requests()) != 0 {
					t.Fatal("question wait skipped after restart")
				}
				if _, err := c.Handle(ctx, ar); err != nil {
					t.Fatal(err)
				}
			}
			snap := waitTask(t, c, a, "completed")
			if len(snap.Tasks) != 1 || snap.Tasks[0].Result != "recovered answer" {
				t.Fatal(snap)
			}
			before, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
			if _, err := c.Handle(ctx, ar); err != nil {
				t.Fatal("retry after task completion", err)
			}
			after, _ := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
			if before.Latest != after.Latest || len(script.Requests()) != 1 {
				t.Fatal("retry replayed work")
			}
			var asks, answers, deliveries, dispatches int
			for _, e := range after.Events {
				switch e.Kind {
				case "question.asked":
					asks++
				case "question.answered":
					answers++
				case "question.delivered":
					deliveries++
				case "tool.dispatching":
					dispatches++
				}
			}
			if asks != 1 || answers != 1 || deliveries != 1 || dispatches != 1 {
				t.Fatal(asks, answers, deliveries, dispatches)
			}
		})
	}
}

func TestQuestionCLIHintRequiresRegisteredExecTool(t *testing.T) {
	script := fixture.New(fixture.Step{Events: fixture.Answer("final", "done"), Check: func(req model.Request) error {
		if strings.Contains(req.System, "question ask") {
			return fmt.Errorf("CLI advertised without execution capability")
		}
		return nil
	}})
	c := loopCore(t, script)
	c.Commands = []CommandHint{{Name: "question.ask", Usage: "zende question ask AGENT --task TASK --file FILE"}}
	a := createWork(t, c, "work")
	runCore(t, c)
	waitTask(t, c, a, "completed")
}

func TestQuestionWaitStopsWithTaskWithoutAnotherModelRequest(t *testing.T) {
	asked := make(chan protocol.Accepted, 1)
	var c *Core
	script := fixture.New(fixture.Step{Events: fixture.Call("ask", "ask_fixture", `{}`)})
	c = loopCore(t, script, testTool{name: "ask_fixture", execute: func(ctx context.Context, call tool.Call) (tool.Result, error) {
		p := protocol.AskQuestion{AgentID: call.AgentID, TaskID: call.TaskID, Prompt: "Continue?"}
		r, _ := protocol.NewRequest("question.ask", call.ID+"-ask", p)
		result, err := c.Handle(ctx, r)
		if err != nil {
			return tool.Result{}, err
		}
		a := result.(protocol.Accepted)
		asked <- a
		return tool.Result{Content: []model.Block{{Type: "text", Text: a.QuestionID}}}, nil
	}})
	a := createWork(t, c, "ask before continuing")
	runCore(t, c)
	var question protocol.Accepted
	select {
	case question = <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("question not created")
	}
	r, _ := protocol.NewRequest("agent.stop", "stop-waiting", protocol.Target{AgentID: a.AgentID})
	if _, err := c.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	snap := waitTask(t, c, a, "cancelled")
	q, err := c.Store.Question(context.Background(), protocol.QuestionTarget{AgentID: a.AgentID, ID: question.QuestionID})
	if err != nil || q.State != "cancelled" || len(script.Requests()) != 1 || len(snap.Questions) != 0 || !snap.Agent.Paused {
		t.Fatal("question waiter ignored stop", q, snap, err)
	}
}
