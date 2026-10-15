package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func frame(kind string, fields map[string]any) string {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = kind
	data, _ := json.Marshal(fields)
	return "event: " + kind + "\ndata: " + string(data) + "\n\n"
}
func begin() string {
	return frame("response.created", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "in_progress"}})
}
func itemEvent(kind string, index int, item any) string {
	return frame("response.output_item."+kind, map[string]any{"output_index": index, "item": item})
}
func complete(items []any, usage any) string {
	return frame("response.completed", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "completed", "output": items, "usage": usage, "error": nil}})
}
func message(id, phase, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "phase": phase, "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{map[string]any{"type": "fixture_annotation", "value": "kept"}}}}}
}
func baseRequest() model.Request {
	return model.Request{ID: "req_fixture", Model: "fixture-model", System: "instructions", MaxOutputTokens: 4096, Messages: []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "do work"}}}}, Tools: []model.Tool{{Name: "act", Description: "Act.", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":[]}`)}}}
}
func collect(t *testing.T, adapter *Adapter, req model.Request, native bool) (model.Response, []model.PublicDelta, error) {
	t.Helper()
	var c model.Collector
	classifier := model.Classifier{Native: native}
	var public []model.PublicDelta
	var formatErr error
	err := adapter.Stream(context.Background(), req, func(d model.Delta) error {
		if err := c.Add(d); err != nil {
			return err
		}
		next, err := classifier.Add(d)
		if err != nil {
			formatErr = err
		}
		public = append(public, next...)
		return nil
	})
	if err != nil {
		return model.Response{}, public, err
	}
	r, err := c.Response()
	if err != nil {
		return r, public, err
	}
	if formatErr != nil {
		return r, public, formatErr
	}
	r.Output, err = classifier.Output()
	return r, public, err
}

func TestHTTPInterleavedCallsPrivateReplayAndLatePhase(t *testing.T) {
	reasoning := map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{map[string]any{"type": "summary_text", "text": "private summary"}}, "encrypted_content": "encrypted-fixture-only", "future_field": map[string]any{"signature": "exact"}}
	call := func(id, args string) map[string]any {
		return map[string]any{"id": "fc_" + id, "call_id": id, "type": "function_call", "name": "act", "arguments": args, "status": "completed"}
	}
	c1, c2 := call("c1", `{ "x": 1 }`), call("c2", `{"x":2}`)
	final := message("m_final", "final_answer", "完成 🟩")
	requests := make(chan map[string]any, 3)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer TEST_ONLY" || r.Header.Get("X-Client-Request-Id") != "req_fixture" {
			t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		if count.Add(1) == 1 {
			io.WriteString(w, begin()+itemEvent("added", 0, map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}})+itemEvent("done", 0, reasoning))
			io.WriteString(w, itemEvent("added", 1, call("c1", ""))+itemEvent("added", 2, call("c2", "")))
			for _, f := range []struct {
				id    string
				index int
				text  string
			}{{"fc_c1", 1, `{ "x":`}, {"fc_c2", 2, `{"x":`}, {"fc_c2", 2, `2}`}, {"fc_c1", 1, ` 1 }`}} {
				io.WriteString(w, frame("response.function_call_arguments.delta", map[string]any{"item_id": f.id, "output_index": f.index, "delta": f.text}))
			}
			io.WriteString(w, itemEvent("done", 2, c2)+itemEvent("done", 1, c1)+complete([]any{reasoning, c1, c2}, map[string]any{"input_tokens": 23, "output_tokens": 9, "output_tokens_details": map[string]any{"reasoning_tokens": 4}}))
		} else {
			io.WriteString(w, begin()+itemEvent("added", 0, map[string]any{"id": "m_final", "type": "message", "role": "assistant", "content": []any{}}))
			io.WriteString(w, frame("response.output_text.delta", map[string]any{"item_id": "m_final", "output_index": 0, "content_index": 0, "delta": "完成 🟩"}))
			io.WriteString(w, itemEvent("done", 0, final)+complete([]any{final}, nil))
		}
	}))
	defer server.Close()
	a, err := New(Config{Endpoint: server.URL + "/v1/", Effort: "high", Credential: func(context.Context) (string, error) { return "TEST_ONLY", nil }})
	if err != nil {
		t.Fatal(err)
	}
	req := baseRequest()
	r, public, err := collect(t, a, req, true)
	if err != nil || len(r.Calls) != 2 || len(r.Messages) != 3 || len(public) != 0 {
		t.Fatalf("response=%+v public=%+v err=%v", r, public, err)
	}
	if r.Calls[0].ID != "c1" || r.Calls[1].ID != "c2" || r.Usage == nil || *r.Usage.InputTokens != 23 || *r.Usage.OutputTokens != 9 {
		t.Fatal(r)
	}
	for _, m := range r.Messages {
		if m.Protocol != Protocol || len(m.Opaque) == 0 {
			t.Fatal("private item lost", m)
		}
	}
	req.Messages = append(req.Messages, r.Messages...)
	req.Messages = append(req.Messages, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "c1", Text: "ok"}}}, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "c2", Text: "bad argument", IsError: true}}})
	r, public, err = collect(t, a, req, true)
	if err != nil {
		t.Fatal(err)
	}
	text, ok, err := r.Final()
	if err != nil || !ok || text != "完成 🟩" || r.Usage != nil {
		t.Fatal(r, err)
	}
	if len(public) != 3 || public[0].Kind != "start" || public[1].Text != text || public[2].Kind != "end" {
		t.Fatal(public)
	}
	first, second := <-requests, <-requests
	if first["store"] != false || first["stream"] != true || first["truncation"] != "disabled" || first["max_output_tokens"] != float64(4096) || first["instructions"] != "instructions" || first["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatal(first)
	}
	if !reflect.DeepEqual(first["include"], []any{"reasoning.encrypted_content"}) || first["tools"].([]any)[0].(map[string]any)["strict"] != false {
		t.Fatal(first)
	}
	input := second["input"].([]any)
	if len(input) != 6 || !reflect.DeepEqual(input[1], reasoning) || !reflect.DeepEqual(input[2], c1) || !reflect.DeepEqual(input[3], c2) || input[5].(map[string]any)["output"] != "Tool error:\nbad argument" {
		t.Fatal(input)
	}
	// Persisted assistant phase and annotations are replayed, not reconstructed.
	req.Messages = append(req.Messages, r.Messages...)
	encoded, err := buildRequest(req, a.endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	var next map[string]any
	json.Unmarshal(encoded, &next)
	last := next["input"].([]any)
	if !reflect.DeepEqual(last[len(last)-1], final) {
		t.Fatal(last)
	}
	for _, change := range []func(*model.Request){func(q *model.Request) { q.Model = "another" }, func(q *model.Request) { q.Messages[1].Protocol = "anthropic" }} {
		copyJSON, _ := json.Marshal(req)
		var q model.Request
		json.Unmarshal(copyJSON, &q)
		change(&q)
		if _, err := buildRequest(q, a.endpoint, ""); err == nil {
			t.Fatal("foreign private data accepted")
		}
	}
	if _, err := buildRequest(req, "https://different.invalid/v1", ""); err == nil {
		t.Fatal("foreign endpoint accepted")
	}
}

func TestHTTPFailureCancellationAndRedaction(t *testing.T) {
	for _, status := range []int{301, 401, 403, 429, 500, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var visits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				visits.Add(1)
				w.Header().Set("Location", "/leak")
				w.WriteHeader(status)
				io.WriteString(w, "SECRET_KEY private prompt")
			}))
			defer server.Close()
			a, _ := New(Config{Endpoint: server.URL})
			err := a.Stream(context.Background(), baseRequest(), func(model.Delta) error { return nil })
			if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), server.URL) || visits.Load() != 1 {
				t.Fatal(err, visits.Load())
			}
		})
	}
	started, closed := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, begin())
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	a, _ := New(Config{Endpoint: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Stream(ctx, baseRequest(), func(model.Delta) error { return nil }) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not interrupt read")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP stream stayed open")
	}
}

func TestRequestRejectsLostContentAndInvalidCalls(t *testing.T) {
	for _, messages := range [][]model.Message{
		{{Role: "user", Content: []model.Block{{Type: "image", Attachment: "local/image"}}}},
		{{Role: "tool", Content: []model.Block{{Type: "text", CallID: "missing", Text: "orphan"}}}},
		{{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "c", Name: "act", Arguments: json.RawMessage(`{}`)}}}},
		{{Role: "assistant", Content: []model.Block{{Type: "text", Text: "text", Attachment: "lost"}}}},
	} {
		req := baseRequest()
		req.Messages = messages
		if _, err := buildRequest(req, "https://fixture.invalid/v1", ""); err == nil {
			t.Fatal("accepted invalid history", messages)
		}
	}
	for _, endpoint := range []string{"", "file:///tmp/a", "https://user:KEY@example.invalid/v1", "https://example.invalid/v1?key=KEY", "https://example.invalid/v1#secret"} {
		if _, err := New(Config{Endpoint: endpoint}); err == nil {
			t.Fatal(endpoint)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("resolved bad credential reached HTTP") }))
	defer server.Close()
	a, _ := New(Config{Endpoint: server.URL, Credential: func(context.Context) (string, error) { return "", errors.New("SECRET") }})
	err := a.Stream(context.Background(), baseRequest(), func(model.Delta) error { return nil })
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Data.Kind != "credential_unavailable" || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

func TestJSONContinuationSurvivesStorageWithoutLosingIntegers(t *testing.T) {
	if !sameJSON([]byte(`{ "patch": "<x>&", "n": 9007199254740993 }`), []byte(`{"n":9007199254740993,"patch":"\u003cx\u003e\u0026"}`)) {
		t.Fatal("equivalent persisted arguments rejected")
	}
	if sameJSON([]byte(`{"n":9007199254740993}`), []byte(`{"n":9007199254740992}`)) {
		t.Fatal("large integer difference lost")
	}
}
