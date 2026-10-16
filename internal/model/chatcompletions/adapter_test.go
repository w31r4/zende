package chatcompletions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func frame(delta any, finish any) string {
	raw, _ := json.Marshal(map[string]any{"id": "chat-test", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(raw) + "\n\n"
}
func call(index int, id, name, args string) map[string]any {
	return map[string]any{"index": index, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}
func calls(values ...map[string]any) map[string]any { return map[string]any{"tool_calls": values} }
func baseRequest() model.Request {
	return model.Request{ID: "req-test", Model: "test-model", System: "instructions", MaxOutputTokens: 2048, Messages: []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "work"}}}}, Tools: []model.Tool{{Name: "act", Description: "Act.", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)}}}
}
func collect(t *testing.T, a *Adapter, request model.Request) (model.Response, []model.PublicMessage, error) {
	t.Helper()
	var collector model.Collector
	var classifier model.Classifier
	err := a.Stream(context.Background(), request, func(d model.Delta) error {
		if err := collector.Add(d); err != nil {
			return err
		}
		classifier.Add(d)
		return nil
	})
	if err != nil {
		return model.Response{}, nil, err
	}
	r, err := collector.Response()
	if err != nil {
		return r, nil, err
	}
	r.Output, err = classifier.Output()
	return r, r.Output, err
}
func requireKind(t *testing.T, err error, kind string) {
	t.Helper()
	var known *protocol.Error
	if !errors.As(err, &known) || known.Data.Kind != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func TestHTTPMultipleRoundsPrivateReplayAndUnknownUsage(t *testing.T) {
	requests := make(chan map[string]any, 3)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer TEST_ONLY" || r.Header.Get("X-Client-Request-Id") != "req-test" {
			t.Error("unexpected request transport")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		switch count.Add(1) {
		case 1:
			io.WriteString(w, frame(map[string]any{"role": "assistant", "reasoning_content": "PRIVATE"}, nil))
			io.WriteString(w, frame(calls(call(0, "c1", "act", `{"x":`), call(1, "c2", "act", `{"x":`)), nil))
			io.WriteString(w, frame(calls(call(1, "", "", `2}`), call(0, "", "", `1}`)), "tool_calls"))
		case 2:
			io.WriteString(w, frame(calls(call(0, "c3", "act", `{"x":3}`)), "tool_calls"))
		case 3:
			for _, part := range []string{"<fi", "nal>\n完成 🟩\n</fin", "al>"} {
				io.WriteString(w, frame(map[string]any{"content": part}, nil))
			}
			io.WriteString(w, frame(map[string]any{}, "stop"))
			io.WriteString(w, "data: {\"id\":\"chat-test\",\"choices\":[],\"usage\":{\"prompt_tokens\":23,\"completion_tokens\":9}}\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	a, err := New(Config{Endpoint: server.URL + "/v1/", ReasoningField: "reasoning_content", IncludeUsage: true, Effort: "high", Credential: func(context.Context) (string, error) { return "TEST_ONLY", nil }})
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	for round := 0; round < 2; round++ {
		response, public, err := collect(t, a, request)
		if err != nil {
			t.Fatal(err)
		}
		want := 2 - round
		if len(response.Calls) != want || len(public) != 0 || response.Usage != nil {
			t.Fatal(response, public)
		}
		if round == 0 && (len(response.Messages[0].Opaque) == 0 || response.Messages[0].Content[0].Text != "") {
			t.Fatal("private reasoning entered visible content or disappeared")
		}
		request.Messages = append(request.Messages, response.Messages...)
		for _, c := range response.Calls {
			request.Messages = append(request.Messages, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: c.ID, Text: "result", IsError: c.ID == "c2"}}})
		}
	}
	response, public, err := collect(t, a, request)
	if err != nil {
		t.Fatal(err)
	}
	text, done, err := response.Final()
	if err != nil || !done || text != "完成 🟩\n" || len(public) != 1 || response.Usage == nil || *response.Usage.InputTokens != 23 || *response.Usage.OutputTokens != 9 {
		t.Fatal(response, err)
	}
	first, second, third := <-requests, <-requests, <-requests
	if first["max_completion_tokens"] != float64(2048) || first["reasoning_effort"] != "high" || first["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatal(first)
	}
	for _, key := range []string{"store", "parallel_tool_calls", "response_format", "max_tokens"} {
		if _, ok := first[key]; ok {
			t.Fatalf("unrequested optional field %s", key)
		}
	}
	messages := second["messages"].([]any)
	if len(messages) != 5 {
		t.Fatal(messages)
	}
	assistant := messages[2].(map[string]any)
	if assistant["reasoning_content"] != "PRIVATE" || assistant["content"] != "" || len(assistant["tool_calls"].([]any)) != 2 || assistant["refusal"] != nil || messages[4].(map[string]any)["content"] != "Tool error:\nresult" {
		t.Fatal(messages)
	}
	if len(third["messages"].([]any)) != 7 {
		t.Fatal(third)
	}
	if third["messages"].([]any)[5].(map[string]any)["reasoning_content"] != "" {
		t.Fatal("declared empty continuation field missing")
	}
	for _, alter := range []func(*Adapter, *model.Request){
		func(a *Adapter, r *model.Request) { a.endpoint = "https://other.invalid" },
		func(a *Adapter, r *model.Request) { r.Model = "other" },
		func(a *Adapter, r *model.Request) { a.config.ReasoningField = "reasoning" },
		func(a *Adapter, r *model.Request) { r.Messages[1].Protocol = "anthropic" },
		func(a *Adapter, r *model.Request) {
			var s privateMessage
			json.Unmarshal(r.Messages[1].Opaque, &s)
			s.Content = "changed"
			r.Messages[1].Opaque, _ = json.Marshal(s)
		},
	} {
		copyAdapter := *a
		raw, _ := json.Marshal(request)
		var copyRequest model.Request
		json.Unmarshal(raw, &copyRequest)
		alter(&copyAdapter, &copyRequest)
		requireKind(t, copyAdapter.ValidateRequest(copyRequest), "protocol_mismatch")
	}
}

func TestCompatibleMinimalRequestAndImageInput(t *testing.T) {
	a, err := New(Config{Endpoint: "https://example.invalid", MaxTokensField: "max_tokens", InstructionRole: "developer", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	request.Tools = nil
	request.Messages = append(request.Messages, model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "rule"}}})
	request.Messages[0].Content = append(request.Messages[0].Content, model.Block{Type: "image", Attachment: "image1"})
	request.Images = map[string]model.Image{"image1": {MediaType: "image/png", Data: []byte("image")}}
	raw, err := a.encode(request, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(raw, &body)
	if len(body) != 4 || body["max_tokens"] != float64(2048) {
		t.Fatal(body)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "developer" || msgs[2].(map[string]any)["role"] != "developer" {
		t.Fatal(msgs)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,aW1hZ2U=" {
		t.Fatal(parts)
	}
	a.config.Images = false
	requireKind(t, a.ValidateRequest(request), "unsupported_capability")
	a.config.Images = true
	delete(request.Images, "image1")
	requireKind(t, a.ValidateRequest(request), "unsupported_capability")
	for _, config := range []Config{{Endpoint: "https://u:secret@example.invalid"}, {Endpoint: "https://example.invalid/?key=secret"}, {Endpoint: "https://example.invalid", MaxTokensField: "typo"}, {Endpoint: "https://example.invalid", InstructionRole: "user"}, {Endpoint: "https://example.invalid", ReasoningField: "content"}} {
		if _, err := New(config); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
}

func TestHistoricalCallsAndPreflight(t *testing.T) {
	var credentials atomic.Int32
	a, _ := New(Config{Endpoint: "https://example.invalid", Credential: func(context.Context) (string, error) { credentials.Add(1); return "secret", nil }})
	request := baseRequest()
	request.Messages = append(request.Messages, model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "c1", Name: "act", Arguments: json.RawMessage(`{"x":1}`)}}})
	if err := a.ValidateContext(request); err != nil {
		t.Fatal(err)
	}
	requireKind(t, a.ValidateRequest(request), "model_protocol")
	request.Messages = append(request.Messages, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "c1", Text: "ok"}}})
	if err := a.ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages, request.Messages[len(request.Messages)-1])
	requireKind(t, a.ValidateRequest(request), "model_protocol")
	if credentials.Load() != 0 {
		t.Fatal("preflight resolved credentials")
	}
}

func TestHTTPFailuresCancellationAndNoRetry(t *testing.T) {
	credentialCtx, cancelCredential := context.WithCancel(context.Background())
	defer cancelCredential()
	cancelledAdapter, _ := New(Config{Endpoint: "https://example.invalid", Credential: func(context.Context) (string, error) { cancelCredential(); return "", errors.New("SECRET") }})
	if err := cancelledAdapter.Stream(credentialCtx, baseRequest(), func(model.Delta) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, status := range []int{301, 401, 403, 429, 500, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var visits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				visits.Add(1)
				w.Header().Set("Location", "/leak")
				w.WriteHeader(status)
				io.WriteString(w, "SECRET private prompt")
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
		t.Fatal("cancel did not return")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("body was not closed")
	}
}
