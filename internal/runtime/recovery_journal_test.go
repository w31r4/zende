package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/tool"
)

func journalWork(t *testing.T) (*Core, protocol.Accepted, func(string, any)) {
	t.Helper()
	c := loopCore(t, nil)
	a := createWork(t, c, "preserve this input")
	if w, err := c.Store.ClaimNext(context.Background(), a.AgentID); err != nil || w == nil {
		t.Fatal(w, err)
	}
	appendEvent := func(kind string, payload any) {
		t.Helper()
		if err := c.Store.AppendTaskEvent(context.Background(), a.AgentID, a.TaskID, kind, payload); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("model.started", requestRecord{ID: "request-original", Model: "test", Protocol: "fixture", RepairLimit: 1})
	return c, a, appendEvent
}

func journalResponse(t *testing.T, c *Core, a protocol.Accepted, calls ...model.ToolCall) {
	t.Helper()
	r := model.Response{Calls: calls}
	for _, call := range calls {
		r.Messages = append(r.Messages, model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: call.ID, Name: call.Name, Arguments: call.Arguments}}})
	}
	if err := c.Store.RecordResponse(context.Background(), a.AgentID, a.TaskID, "request-original", r); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryJournalDispatchBoundariesAreReadOnly(t *testing.T) {
	for _, stage := range []string{"stream", "planned", "prepared", "dispatched", "unknown", "legacy", "skipped", "completed", "final"} {
		t.Run(stage, func(t *testing.T) {
			c, a, appendEvent := journalWork(t)
			call := model.ToolCall{ID: "model-call", Name: "exec_command", Arguments: json.RawMessage(`{"command":"irreversible-fixture"}`)}
			op := invocation{SchemaVersion: 2, ID: "operation-original", RequestID: "request-original", Call: call, Version: "2"}
			wantNext, wantCall := "resume_calls", stage
			switch stage {
			case "stream":
				appendEvent("model.delta", map[string]any{"request_id": op.RequestID, "delta": model.Delta{Kind: model.ArgumentsDelta, CallID: call.ID, Text: `{"command":`}})
				wantNext, wantCall = "continue_model", ""
			case "final":
				response := model.Response{Output: []model.PublicMessage{{Category: "final", Text: "saved answer"}}}
				if err := c.Store.RecordResponse(context.Background(), a.AgentID, a.TaskID, op.RequestID, response); err != nil {
					t.Fatal(err)
				}
				wantNext, wantCall = "finish", ""
			default:
				journalResponse(t, c, a, call)
				if stage != "planned" {
					if stage == "legacy" {
						op.SchemaVersion = 0
					}
					appendEvent("tool.started", op)
				}
				if stage == "dispatched" || stage == "unknown" || stage == "completed" {
					appendEvent("tool.dispatching", op)
				}
				switch stage {
				case "legacy":
					wantNext, wantCall = "verify", "unknown"
				case "dispatched":
					wantNext = "verify"
				case "unknown":
					appendEvent("tool.unknown", toolOutcome{Invocation: op, Result: tool.Result{ProcessID: op.ID}, Error: protocol.E("operation_unknown", "result not committed")})
					wantNext = "verify"
				case "skipped":
					appendEvent("tool.skipped", op)
				case "completed":
					outcome := toolOutcome{Invocation: op, Result: tool.Result{ProcessID: op.ID}}
					if err := c.Store.RecordToolResult(context.Background(), a.AgentID, a.TaskID, op.RequestID, outcome, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: call.ID, Text: "saved result"}}}); err != nil {
						t.Fatal(err)
					}
					wantNext = "continue_model"
				}
			}
			if err := c.Store.HoldInterrupted(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, _ := c.Store.Snapshot(context.Background(), a.AgentID)
			for range 2 {
				report, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID})
				if err != nil || report.NextStep != wantNext || report.TaskID != a.TaskID {
					t.Fatal(report, err)
				}
				if wantCall == "" {
					if len(report.Calls) != 0 {
						t.Fatal(report)
					}
				} else if len(report.Calls) != 1 || report.Calls[0].State != wantCall || wantCall != "planned" && report.Calls[0].OperationID != op.ID {
					t.Fatal(report)
				}
				if (stage == "unknown" || stage == "completed") && report.Calls[0].ProcessID != op.ID {
					t.Fatal("original process reference was lost", report)
				}
			}
			after, _ := c.Store.Snapshot(context.Background(), a.AgentID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("read-only recovery changed task state or journal", before, after)
			}
		})
	}
}

func TestRecoveryJournalRejectsChangedIdentityAndIncompleteResults(t *testing.T) {
	for _, scenario := range []string{"operation", "call", "arguments", "version", "schema", "unpaired", "next-request", "repaired-without-repair"} {
		t.Run(scenario, func(t *testing.T) {
			c, a, appendEvent := journalWork(t)
			call := model.ToolCall{ID: "original", Name: "exec_command", Arguments: json.RawMessage(`{"n":9007199254740993}`)}
			journalResponse(t, c, a, call)
			op := invocation{SchemaVersion: 2, ID: "operation", RequestID: "request-original", Call: call, Version: "2"}
			appendEvent("tool.started", op)
			kind := "tool.dispatching"
			switch scenario {
			case "operation":
				op.ID = "replacement"
			case "call":
				op.Call.ID = "replacement"
			case "arguments":
				op.Call.Arguments = json.RawMessage(`{"n":9007199254740992}`)
			case "version":
				op.Version = "3"
			case "schema":
				op.SchemaVersion = 1
			case "unpaired":
				appendEvent("tool.completed", toolOutcome{Invocation: op})
				kind = ""
			case "next-request":
				appendEvent("model.started", requestRecord{ID: "new-request"})
				kind = ""
			case "repaired-without-repair":
				appendEvent("model.output.repaired", map[string]any{"request_id": op.RequestID, "attempt": 0})
				kind = ""
			}
			if kind != "" {
				appendEvent(kind, op)
			}
			if _, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID}); err == nil {
				t.Fatal("inconsistent journal accepted", scenario)
			}
		})
	}
}

func TestRecoveryJournalRestoresRepairBudgetAndCancellation(t *testing.T) {
	c, a, appendEvent := journalWork(t)
	appendEvent("model.output.invalid", map[string]any{"request_id": "request-original", "error": protocol.E("output_format", "interrupted text")})
	if err := c.Store.RecordFormatRepair(context.Background(), a.AgentID, a.TaskID, "request-original", 1, 1, "repair only"); err != nil {
		t.Fatal(err)
	}
	appendEvent("model.started", requestRecord{ID: "repair-request", FormatRepair: true, RepairAttempt: 1, RepairLimit: 1})
	if err := c.Store.HoldInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := readRecoveryJournal(context.Background(), c.Store, a.AgentID, a.TaskID)
	if err != nil || j.RepairCount != 1 || j.RepairLimit != 1 || !j.RepairOnly || j.report().NextStep != "continue_model" {
		t.Fatal(j, err)
	}

	c, a, _ = journalWork(t)
	p := protocol.Target{AgentID: a.AgentID}
	req, _ := protocol.NewRequest("agent.stop", "stop-original", p)
	if _, err := c.Store.Stop(context.Background(), req, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.HoldInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID})
	if err != nil || !r.Paused || r.NextStep != "settle_cancellation" || r.TaskState != "cancelling" {
		t.Fatal(r, err)
	}
}

func TestRecoveryJournalSelectsExplicitTaskAndRejectsCrossAgent(t *testing.T) {
	c, a, _ := journalWork(t)
	queued := submitWork(t, c, a.AgentID, "queued")
	r, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID})
	if err != nil || r.TaskID != a.TaskID || r.NextStep != "observe" {
		t.Fatal(r, err)
	}
	r, err = c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: a.AgentID, TaskID: queued.TaskID})
	if err != nil || r.NextStep != "queued" {
		t.Fatal(r, err)
	}
	other := createWork(t, c, "other agent")
	if _, err := c.Recovery(context.Background(), protocol.RecoveryTarget{AgentID: other.AgentID, TaskID: a.TaskID}); err == nil {
		t.Fatal("task from another agent accepted")
	}
}

func TestDispatchMarkerPrecedesExecutor(t *testing.T) {
	var c *Core
	seen := make(chan string, 1)
	executor := testTool{"effect", func(ctx context.Context, call tool.Call) (tool.Result, error) {
		r, err := c.Recovery(ctx, protocol.RecoveryTarget{AgentID: call.AgentID, TaskID: call.TaskID})
		if err != nil || len(r.Calls) != 1 || r.Calls[0].OperationID != call.ID || r.Calls[0].State != "dispatched" || r.Calls[0].DispatchSeq <= r.Calls[0].IntentSeq {
			seen <- fmt.Sprintf("dispatch was not durable: %+v %v", r, err)
		} else {
			seen <- "confirmed"
		}
		return tool.Result{}, nil
	}}
	c = loopCore(t, fixture.New(fixture.Step{Events: fixture.Call("call", "effect", `{}`)}, fixture.Step{Events: fixture.Answer("final", "done")}), executor)
	a := createWork(t, c, "execute once")
	runCore(t, c)
	waitTask(t, c, a, "completed")
	if got := <-seen; got != "confirmed" {
		t.Fatal(got)
	}
}
