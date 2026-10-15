package responses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
)

type chunkReader struct {
	reader io.Reader
	width  int
}

func (r chunkReader) Read(p []byte) (int, error) { return r.reader.Read(p[:min(len(p), r.width)]) }

func decodeStream(stream string, width int) (model.Response, error) {
	var c model.Collector
	d := decoder{endpoint: "https://fixture.invalid/v1", model: "fixture", emit: c.Add}
	if err := readSSE(chunkReader{strings.NewReader(stream), width}, d.receive); err != nil {
		return model.Response{}, err
	}
	return c.Response()
}

func TestSSEFragmentationLineEndingsAndContentParts(t *testing.T) {
	item := message("m", "final_answer", "")
	item["content"] = []any{map[string]any{"type": "output_text", "text": "中文"}, map[string]any{"type": "refusal", "refusal": " 🟩"}}
	stream := begin() + itemEvent("added", 0, map[string]any{"id": "m", "type": "message", "role": "assistant", "phase": "final_answer", "content": []any{}})
	// A later content part can arrive first without changing visible order.
	stream += frame("response.refusal.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 1, "delta": " 🟩"})
	stream += frame("response.output_text.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "delta": "中文"})
	stream += frame("response.output_text.done", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "text": "中文"})
	stream += frame("response.content_part.done", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "中文"}})
	stream += frame("response.refusal.done", map[string]any{"item_id": "m", "output_index": 0, "content_index": 1, "refusal": " 🟩"})
	stream += itemEvent("done", 0, item) + complete([]any{item}, map[string]any{"input_tokens": 0})
	var first model.Response
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, width := range []int{1, 2, 3, 7, 4096} {
			r, err := decodeStream(strings.ReplaceAll("\ufeff:keepalive\n\n"+stream, "\n", ending), width)
			if err != nil {
				t.Fatal(ending, width, err)
			}
			if r.Messages[0].Content[0].Text != "中文 🟩" || r.Messages[0].Category != "final" || r.Usage.InputTokens == nil || *r.Usage.InputTokens != 0 || r.Usage.OutputTokens != nil {
				t.Fatal(r)
			}
			if first.Messages == nil {
				first = r
			} else if !reflect.DeepEqual(first, r) {
				t.Fatal("network fragmentation changed response")
			}
		}
	}
	var received string
	err := readSSE(strings.NewReader("id: ignore\nretry: 20\ndata: {\ndata: \"type\":\"done\"}\n\n"), func(_ string, data []byte) (bool, error) { received = string(data); return true, nil })
	if err != nil || received != "{\n\"type\":\"done\"}" {
		t.Fatal(received, err)
	}
}

func TestMalformedStreamsNeverAuthorizeCalls(t *testing.T) {
	call := func(args string) map[string]any {
		return map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "act", "arguments": args, "status": "completed"}
	}
	start := begin() + itemEvent("added", 0, call(""))
	partial := frame("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc", "delta": `{"x":`})
	item := message("m", "final_answer", "done")
	textStart := begin() + itemEvent("added", 0, map[string]any{"id": "m", "type": "message", "role": "assistant"})
	for name, stream := range map[string]string{
		"eof":                  start + partial,
		"sentinel":             start + partial + "data: [DONE]\n\n",
		"complete_before_item": start + complete([]any{call(`{}`)}, nil),
		"changed_arguments":    start + partial + itemEvent("done", 0, call(`{"y":1}`)) + complete([]any{call(`{"y":1}`)}, nil),
		"array_arguments":      start + itemEvent("done", 0, call(`[]`)) + complete([]any{call(`[]`)}, nil),
		"duplicate_arguments":  start + itemEvent("done", 0, call(`{"x":1,"x":2}`)),
		"wrong_item":           start + frame("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "other", "delta": "{}"}),
		"missing_index":        start + frame("response.function_call_arguments.delta", map[string]any{"item_id": "fc", "delta": "{}"}),
		"changed_text":         textStart + frame("response.output_text.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "delta": "different"}) + itemEvent("done", 0, item),
		"lost_sparse_part":     textStart + frame("response.output_text.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 99, "delta": "lost"}) + itemEvent("done", 0, item),
		"changed_complete":     textStart + itemEvent("done", 0, item) + complete([]any{message("m", "final_answer", "changed")}, nil),
		"incomplete":           begin() + frame("response.incomplete", nil),
		"failed":               begin() + frame("response.failed", nil),
		"error":                begin() + frame("error", map[string]any{"message": "SECRET"}),
		"unknown_item":         begin() + itemEvent("added", 0, map[string]any{"id": "x", "type": "computer_call"}),
		"unknown_event":        begin() + frame("response.unknown", nil),
		"duplicate_fields":     "data: {\"type\":\"error\",\"type\":\"response.completed\"}\n\n",
		"name_mismatch":        "event: response.completed\ndata: {\"type\":\"error\"}\n\n",
		"negative_usage":       textStart + itemEvent("done", 0, item) + complete([]any{item}, map[string]any{"input_tokens": -1}),
		"partial_last_frame":   textStart + itemEvent("done", 0, item) + strings.TrimSuffix(complete([]any{item}, nil), "\n"),
		"repeated_sequence":    frame("response.created", map[string]any{"sequence_number": 0, "response": map[string]any{"id": "resp_fixture"}}) + frame("response.in_progress", map[string]any{"sequence_number": 0, "response": map[string]any{"id": "resp_fixture"}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeStream(stream, 1); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid stream", err)
			}
		})
	}
}

func TestDoneSuffixAndAbsentPhaseRemainExplicit(t *testing.T) {
	item := message("m", "", "unclassified")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, begin()+itemEvent("added", 0, map[string]any{"id": "m", "type": "message", "role": "assistant"})+frame("response.output_text.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "delta": "un"})+itemEvent("done", 0, item)+complete([]any{item}, nil))
	}))
	defer server.Close()
	a, _ := New(Config{Endpoint: server.URL})
	r, _, err := collect(t, a, baseRequest(), true)
	if err == nil || len(r.Messages) != 1 || r.Messages[0].Content[0].Text != "unclassified" {
		t.Fatal(r, err)
	}
	// A consumer failure must propagate without becoming provider success.
	want := errors.New("storage failure")
	err = a.Stream(context.Background(), baseRequest(), func(model.Delta) error { return want })
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestSSEAndAssemblyLimits(t *testing.T) {
	for _, stream := range []string{"data: " + strings.Repeat("x", maxEventBytes) + "\n\n", strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", maxEventBytes/1024+1) + "\n", "data: \xff\n\n"} {
		if err := readSSE(strings.NewReader(stream), func(string, []byte) (bool, error) { return false, nil }); err == nil {
			t.Fatal("unbounded or invalid SSE")
		}
	}
	item := message("m", "final_answer", "")
	stream := begin() + itemEvent("added", 0, item) + frame("response.output_text.delta", map[string]any{"item_id": "m", "output_index": 0, "content_index": 0, "delta": strings.Repeat("x", model.MaxBlockBytes+1)})
	if _, err := decodeStream(stream, 4096); err == nil {
		t.Fatal("unbounded assembly")
	}
}

func FuzzSSEChunking(f *testing.F) {
	for _, s := range []string{"data: {}\n\n", "\ufeff: keepalive\r\ndata: {}\r\n\r\n", "data: [DONE]\r\r", "data: {\ndata: }\n\n"} {
		f.Add(s, uint8(1))
	}
	f.Fuzz(func(t *testing.T, s string, width uint8) {
		if len(s) > 65536 {
			t.Skip()
		}
		run := func(width int) ([]string, bool) {
			var frames []string
			err := readSSE(chunkReader{strings.NewReader(s), width}, func(name string, data []byte) (bool, error) {
				frames = append(frames, name+":"+string(data))
				return json.Valid(data), nil
			})
			return frames, err == nil
		}
		a, ok := run(max(1, len(s)))
		b, other := run(int(width) + 1)
		if ok != other || !reflect.DeepEqual(a, b) {
			t.Fatalf("chunk-dependent SSE: %v %v", a, b)
		}
	})
}
