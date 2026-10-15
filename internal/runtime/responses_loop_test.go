package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/tool"
)

func responsesFrame(kind string, fields map[string]any) string {
	fields["type"] = kind
	data, _ := json.Marshal(fields)
	return "event: " + kind + "\ndata: " + string(data) + "\n\n"
}
func responsesRound(item map[string]any, partial bool) string {
	response := map[string]any{"id": "r_fixture", "status": "in_progress"}
	start := map[string]any{}
	for k, v := range item {
		start[k] = v
	}
	start["status"] = "in_progress"
	if item["type"] == "function_call" {
		start["arguments"] = ""
	} else {
		start["content"] = []any{}
	}
	text := responsesFrame("response.created", map[string]any{"response": response}) + responsesFrame("response.output_item.added", map[string]any{"output_index": 0, "item": start})
	if item["type"] == "function_call" {
		args := item["arguments"].(string)
		if partial {
			args = `{"x":`
		}
		for _, part := range []string{args[:len(args)/2], args[len(args)/2:]} {
			text += responsesFrame("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": item["id"], "delta": part})
		}
	}
	if partial {
		return text
	}
	text += responsesFrame("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	return text + responsesFrame("response.completed", map[string]any{"response": map[string]any{"id": "r_fixture", "status": "completed", "output": []any{item}}})
}

func configureResponses(t *testing.T, c *Core, url string, native bool) {
	t.Helper()
	cfg := c.Config.Config
	cfg.Model = "test/model"
	cfg.Connections["test"] = resource.Connection{Protocol: "responses", BaseURL: url, Models: []resource.ModelConfig{{ID: "model", ContextWindow: 32000, MaxOutput: 4096, NativeCategories: native}}}
	var err error
	c.Models, err = provider.New(cfg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c.Config.Config = cfg
}

func TestResponsesLoopTwoRoundsErrorAndPersistedPrivateItems(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			var calls, requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input        []map[string]any `json:"input"`
					Instructions string           `json:"instructions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				n := requests.Add(1)
				if n > 1 {
					last := body.Input[len(body.Input)-1]
					want := fmt.Sprintf("call%d", n-1)
					if last["type"] != "function_call_output" || last["call_id"] != want {
						t.Error("tool result missing", last)
					}
					if n == 3 && !strings.Contains(last["output"].(string), "fixture_failure") {
						t.Error("tool error missing", last)
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n < 3 {
					item := map[string]any{"id": fmt.Sprintf("fc%d", n), "call_id": fmt.Sprintf("call%d", n), "type": "function_call", "name": "act", "arguments": `{ "x": 1 }`, "status": "completed"}
					io.WriteString(w, responsesRound(item, false))
					return
				}
				if n > 3 {
					t.Error("unexpected retry")
				}
				text, phase := "done", "final_answer"
				if !native {
					text = "<final>\ndone\n</final>"
					phase = ""
				}
				io.WriteString(w, responsesRound(map[string]any{"id": "answer", "type": "message", "role": "assistant", "phase": phase, "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}, false))
			}))
			defer server.Close()
			c := loopCore(t, nil, testTool{"act", func(_ context.Context, call tool.Call) (tool.Result, error) {
				if calls.Add(1) == 2 {
					return tool.Result{}, protocol.E("fixture_failure", "expected tool error")
				}
				return tool.Result{Content: []model.Block{{Type: "text", Text: "ok"}}}, nil
			}})
			configureResponses(t, c, server.URL, native)
			a := createWork(t, c, "perform two rounds")
			runCore(t, c)
			snap := waitTask(t, c, a, "completed")
			if calls.Load() != 2 || requests.Load() != 3 || strings.TrimSpace(snap.Tasks[0].Result) != "done" {
				t.Fatal(calls.Load(), requests.Load(), snap)
			}
			messages, _, err := c.Store.ModelContext(context.Background(), a.AgentID)
			if err != nil || len(messages) != 6 {
				t.Fatal(messages, err)
			}
			for _, index := range []int{1, 3, 5} {
				if messages[index].Protocol != "responses" || len(messages[index].Opaque) == 0 {
					t.Fatal("protocol continuation lost", messages[index])
				}
			}
			page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			start, result := 0, 0
			for _, e := range page.Events {
				if e.Kind == "tool.started" {
					start++
				}
				if e.Kind == "tool.completed" {
					result++
				}
			}
			if start != 2 || result != 2 {
				t.Fatal(start, result)
			}
		})
	}
}

func TestResponsesPartialArgumentsNeverExecute(t *testing.T) {
	var calls, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, responsesRound(map[string]any{"id": "fc", "call_id": "call", "name": "act", "type": "function_call", "arguments": "{}", "status": "completed"}, true))
	}))
	defer server.Close()
	c := loopCore(t, nil, testTool{"act", func(context.Context, tool.Call) (tool.Result, error) { calls.Add(1); return tool.Result{}, nil }})
	configureResponses(t, c, server.URL, true)
	a := createWork(t, c, "do not execute incomplete arguments")
	runCore(t, c)
	waitTask(t, c, a, "failed")
	if calls.Load() != 0 || requests.Load() != 1 {
		t.Fatal(calls.Load(), requests.Load())
	}
	page, err := c.Store.Events(context.Background(), protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	partial := false
	for _, e := range page.Events {
		if e.Kind == "tool.started" {
			t.Fatal("partial call dispatched")
		}
		if e.Kind == "model.delta" && strings.Contains(string(e.Payload), "arguments.delta") {
			partial = true
		}
	}
	if !partial {
		t.Fatal("partial parameters were not retained")
	}
}
