package chatcompletions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
)

func TestDecoderRejectsIncompleteCallsAndUnexpectedSemantics(t *testing.T) {
	valid := frame(calls(call(0, "c1", "act", `{"x":1}`)), "tool_calls")
	for name, input := range map[string]string{
		"empty": "", "sentinel": "data: [DONE]\n\n", "no-sentinel": valid,
		"partial-event": valid + "data: [DONE]\n", "partial-arguments": frame(calls(call(0, "c1", "act", `{"x":`)), "tool_calls") + "data: [DONE]\n\n",
		"duplicate-arguments":    frame(calls(call(0, "c1", "act", `{"x":1,"x":2}`)), "tool_calls") + "data: [DONE]\n\n",
		"array-arguments":        frame(calls(call(0, "c1", "act", `[]`)), "tool_calls") + "data: [DONE]\n\n",
		"missing-identity":       frame(calls(call(0, "", "act", `{}`)), "tool_calls") + "data: [DONE]\n\n",
		"repeated-id":            frame(calls(call(0, "c1", "act", `{}`), call(1, "c1", "act", `{}`)), "tool_calls") + "data: [DONE]\n\n",
		"changed-id":             frame(calls(call(0, "c1", "act", ``)), nil) + frame(calls(call(0, "c2", "act", `{}`)), "tool_calls") + "data: [DONE]\n\n",
		"changed-name":           frame(calls(call(0, "c1", "act", ``)), nil) + frame(calls(call(0, "", "other", `{}`)), "tool_calls") + "data: [DONE]\n\n",
		"gap":                    frame(calls(call(1, "c1", "act", `{}`)), "tool_calls") + "data: [DONE]\n\n",
		"stop-with-call":         frame(calls(call(0, "c1", "act", `{}`)), "stop") + "data: [DONE]\n\n",
		"call-without-call":      frame(map[string]any{}, "tool_calls") + "data: [DONE]\n\n",
		"after-finish":           valid + frame(map[string]any{"content": "late"}, nil) + "data: [DONE]\n\n",
		"truncated":              frame(map[string]any{"content": "<final>\nok\n</final>"}, "length") + "data: [DONE]\n\n",
		"filtered":               frame(map[string]any{}, "content_filter") + "data: [DONE]\n\n",
		"legacy":                 frame(map[string]any{"function_call": map[string]any{"name": "act", "arguments": "{}"}}, "function_call") + "data: [DONE]\n\n",
		"unconfigured-reasoning": frame(map[string]any{"reasoning_content": "private"}, nil),
		"audio":                  frame(map[string]any{"audio": map[string]any{"data": "private"}}, nil),
		"role":                   frame(map[string]any{"role": "user"}, "stop"),
		"identity-change":        valid + "data: {\"id\":\"other\",\"choices\":[],\"usage\":{}}\n\n",
		"usage-negative":         "data: {\"id\":\"chat-test\",\"choices\":[],\"usage\":{\"prompt_tokens\":-1}}\n\n",
		"duplicate-json":         "data: {\"id\":\"a\",\"id\":\"b\",\"choices\":[]}\n\n",
		"missing-index":          "data: {\"id\":\"a\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"extra-choice":           "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{}},{\"index\":1,\"delta\":{}}]}\n\n",
		"invalid-utf8":           "data: \xff\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			var collector model.Collector
			var ended bool
			d := decoder{emit: func(delta model.Delta) error {
				ended = ended || delta.Kind == model.ResponseEnd
				return collector.Add(delta)
			}}
			if err := readSSE(strings.NewReader(input), d.receive); err == nil {
				t.Fatal("accepted invalid stream")
			}
			if _, err := collector.Response(); err == nil || ended {
				t.Fatal("invalid stream authorized execution")
			}
		})
	}
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestSSEFragmentationExplicitClassificationAndBudgets(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(ending, "\n", "LF"), "\r", "CR"), func(t *testing.T) {
			stream := "\ufeff: heartbeat\n\n" + frame(map[string]any{"content": "ordinary text"}, "stop") + "data: [DONE]\n\n"
			stream = strings.ReplaceAll(stream, "\n", ending)
			var collector model.Collector
			var classifier model.Classifier
			d := decoder{emit: func(delta model.Delta) error {
				if err := collector.Add(delta); err != nil {
					return err
				}
				classifier.Add(delta)
				return nil
			}}
			if err := readSSE(oneByteReader{strings.NewReader(stream)}, d.receive); err != nil {
				t.Fatal(err)
			}
			response, err := collector.Response()
			if err != nil {
				t.Fatal(err)
			}
			if _, done, err := response.Final(); err != nil || done {
				t.Fatal("finish_reason inferred final")
			}
			if _, err := classifier.Output(); err == nil {
				t.Fatal("unmarked text accepted as classified")
			}
		})
	}
	var collector model.Collector
	d := decoder{emit: collector.Add}
	input := frame(map[string]any{"content": strings.Repeat("x", model.MaxBlockBytes+1)}, "stop") + "data: [DONE]\n\n"
	requireKind(t, readSSE(strings.NewReader(input), d.receive), "output_limit")
	stop := errors.New("consumer stopped")
	d = decoder{emit: func(model.Delta) error { return stop }}
	if err := readSSE(strings.NewReader(frame(map[string]any{"content": "text"}, nil)), d.receive); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestRefusalReplayAndDelayedCallIdentity(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "late-identity", true: "refusal"}[refusal], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if refusal {
					io.WriteString(w, frame(map[string]any{"refusal": "<final>\nCannot do that.\n</final>"}, "stop"))
				} else {
					io.WriteString(w, frame(calls(call(0, "", "", `{"x":`)), nil)+frame(calls(call(0, "c1", "act", `1}`)), "tool_calls"))
				}
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			a, _ := New(Config{Endpoint: server.URL})
			response, _, err := collect(t, a, baseRequest())
			if err != nil {
				t.Fatal(err)
			}
			request := baseRequest()
			request.Messages = append(request.Messages, response.Messages...)
			if refusal {
				text, done, err := response.Final()
				if err != nil || !done || text != "Cannot do that.\n" {
					t.Fatal(response, err)
				}
				raw, err := a.encode(request, false)
				if err != nil || !strings.Contains(string(raw), `"refusal":`) {
					t.Fatal(string(raw), err)
				}
			} else {
				if len(response.Calls) != 1 || string(response.Calls[0].Arguments) != `{"x":1}` {
					t.Fatal(response)
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if err := a.Stream(ctx, request, func(model.Delta) error { return nil }); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
		})
	}
}
