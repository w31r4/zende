package render

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func historyFixture(t *testing.T) []protocol.Event {
	t.Helper()
	data, err := os.ReadFile("testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestHistoryProjectionAndFoldPreserveFacts(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	events := historyFixture(t)
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent", Cwd: "/project"}, Cursor: 16}})
	for _, event := range events {
		m, _ = update(t, m, coreEventMsg{event})
		if m.err != nil {
			t.Fatal(event.Seq, m.err)
		}
		if event.Seq == 14 {
			if m.transcript.Entries[0].State != "running" || m.transcript.Entries[3].State != "streaming" {
				t.Fatal("message end falsely completed task")
			}
		}
	}
	p := m.transcript
	if p.Cursor != 16 || p.Agent.State != "idle" || len(p.Entries) != 4 {
		t.Fatal(p)
	}
	want := []Entry{
		{Key: "task:task", Kind: "user", AgentID: "agent", TaskID: "task", State: "completed", Text: "检查文件", Seq: 2, LastSeq: 16},
		{Key: "output:request1:message1:0", Kind: "assistant", AgentID: "agent", TaskID: "task", RequestID: "request1", MessageID: "message1", Category: "commentary", State: "confirmed", Text: "正在检查中文 é 🙂", Seq: 5, LastSeq: 8},
		{Key: "tool:operation", Kind: "tool", AgentID: "agent", TaskID: "task", RequestID: "request1", OperationID: "operation", CallID: "call", Name: "exec_command", ProcessID: "process", State: "completed", Text: "/project", Seq: 9, LastSeq: 11},
		{Key: "output:request2:message2:0", Kind: "assistant", AgentID: "agent", TaskID: "task", RequestID: "request2", MessageID: "message2", Category: "final", State: "confirmed", Text: "检查完成。", Seq: 12, LastSeq: 15},
	}
	if !reflect.DeepEqual(p.Entries, want) {
		t.Fatalf("projection mismatch\ngot %#v\nwant %#v", p.Entries, want)
	}
	before, _ := json.Marshal(p)
	if !strings.Contains(m.View().Content, "正在检查中文") {
		t.Fatal("commentary missing")
	}
	m.input.SetValue("/commentary")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || !m.foldCommentary || strings.Contains(m.View().Content, "正在检查中文") || !strings.Contains(m.View().Content, "检查完成。") {
		t.Fatal("fold failed")
	}
	after, _ := json.Marshal(m.transcript)
	if string(before) != string(after) {
		t.Fatal("fold mutated history")
	}
	for _, event := range events {
		m, _ = update(t, m, coreEventMsg{event})
	}
	after, _ = json.Marshal(m.transcript)
	if string(before) != string(after) {
		t.Fatal("duplicate events mutated history")
	}
}

func appendProjection(t *testing.T, p *Transcript, kind string, payload any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(protocol.Event{Version: 1, AgentID: "agent", TaskID: "task", Seq: p.Cursor + 1, Kind: kind, Payload: data}); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidInterruptedAndPrivateOutputs(t *testing.T) {
	for _, kind := range []string{"model.output.invalid", "model.interrupted", "task.failed", "task.unknown", "task.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			p := Transcript{Agent: protocol.Agent{ID: "agent"}}
			for _, e := range historyFixture(t)[:14] {
				if err := p.Apply(e); err != nil {
					t.Fatal(err)
				}
			}
			appendProjection(t, &p, kind, map[string]any{"request_id": "request2", "error": protocol.E("test_failure", "interrupted")})
			for _, entry := range p.Entries {
				if entry.Category == "final" && entry.State == "confirmed" {
					t.Fatal("invalid final confirmed")
				}
			}
		})
	}
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	appendProjection(t, &p, "model.delta", map[string]any{"request_id": "r", "delta": model.Delta{Kind: model.ProtocolData, Raw: json.RawMessage(`{"encrypted":"private-token"}`)}})
	appendProjection(t, &p, "context.message", map[string]any{"message": model.Message{Role: "developer", Content: []model.Block{{Type: "text", Text: "private-prompt"}}}})
	if len(p.Entries) != 0 {
		t.Fatal("private context leaked")
	}
	appendProjection(t, &p, "model.delta", map[string]any{"request_id": "r", "delta": model.Delta{Kind: model.TextDelta, MessageID: "m", Text: "literal uncategorized"}})
	if p.Entries[0].Category != "unclassified" {
		t.Fatal(p.Entries)
	}
	appendProjection(t, &p, "model.output.delta", map[string]any{"request_id": "r", "delta": model.PublicDelta{MessageID: "m", Kind: "start", Category: "final"}})
	appendProjection(t, &p, "model.output.delta", map[string]any{"request_id": "r", "delta": model.PublicDelta{MessageID: "m", Kind: "text", Category: "final", Text: "literal uncategorized"}})
	if !p.Entries[0].Hidden || strings.Count(strings.Join(p.Lines(80, 24, 0, false), "\n"), "literal uncategorized") != 1 {
		t.Fatal("raw and categorized text duplicated")
	}
}

func TestProjectionBoundsAndInvalidEventCursor(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	for i := 0; i < transcriptEntries+100; i++ {
		appendProjection(t, &p, "model.output.invalid", map[string]any{"request_id": "r", "reason": "bad"})
	}
	if len(p.Entries) != transcriptEntries || p.DroppedThrough != 100 {
		t.Fatal(len(p.Entries), p.DroppedThrough)
	}
	for i := 0; i < 100; i++ {
		appendProjection(t, &p, "model.delta", map[string]any{"request_id": "r", "delta": model.Delta{Kind: model.TextDelta, MessageID: protocol.NewID("m"), Text: strings.Repeat("中文", entryBytes)}})
	}
	if p.bytes > transcriptBytes || len(p.Entries) > transcriptEntries {
		t.Fatal("unbounded display")
	}
	for _, entry := range p.Entries {
		if len(entry.Text) > entryBytes || !utf8.ValidString(entry.Text) {
			t.Fatal("invalid retained text")
		}
	}
	before, _ := json.Marshal(p)
	for _, event := range []protocol.Event{
		{Version: 1, AgentID: "other", Seq: p.Cursor + 1},
		{Version: 2, AgentID: "agent", Seq: p.Cursor + 1},
		{Version: 1, AgentID: "agent", Seq: p.Cursor + 2},
		{Version: 1, AgentID: "agent", Seq: p.Cursor + 1, Kind: "model.output.delta", Payload: json.RawMessage(`{"request_id":"r","delta":{"category":"final","kind":"oops"}}`)},
	} {
		if err := p.Apply(event); err == nil {
			t.Fatal("invalid event accepted")
		}
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("rejected event moved cursor or state")
	}
	if len(p.Lines(48, 13, 10000, false)) == 0 {
		t.Fatal("overscrolled into empty history")
	}
}

func TestToolRecoveryProjectionAndSplitRawUTF8(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	op := map[string]any{"operation_id": "op", "request_id": "r", "call": model.ToolCall{ID: "call", Name: "write_stdin"}}
	appendProjection(t, &p, "tool.started", op)
	appendProjection(t, &p, "tool.dispatching", op)
	appendProjection(t, &p, "tool.unknown", map[string]any{"operation_id": "op", "reason": "result not committed"})
	if p.Entries[0].State != "unknown" || p.Entries[0].CallID != "call" || p.Entries[0].Name != "write_stdin" {
		t.Fatal(p.Entries)
	}
	appendProjection(t, &p, "tool.completed", map[string]any{"invocation": op, "result": map[string]any{"process_id": "p"}, "error": protocol.E("tool_failed", "known failure")})
	if p.Entries[0].State != "failed" || !strings.Contains(p.Entries[0].Text, "known failure") {
		t.Fatal(p.Entries)
	}
	appendProjection(t, &p, "tool.completed", map[string]any{"invocation": op, "result": map[string]any{"process_id": "p"}})
	if len(p.Entries) != 1 || p.Entries[0].State != "completed" || p.Entries[0].Text != "" {
		t.Fatal("stale failure retained", p.Entries)
	}
	for _, b := range []byte("中文") {
		appendProjection(t, &p, "model.delta", map[string]any{"request_id": "r", "delta": model.Delta{Kind: model.TextDelta, MessageID: "m", Text: "\uFFFD"}, "text_bytes": []byte{b}})
	}
	if p.Entries[1].Text != "中文" || p.Entries[1].Category != "unclassified" {
		t.Fatal("raw bytes lost", p.Entries[1])
	}
	appendProjection(t, &p, "future.event", map[string]any{"private": "do not show"})
	if len(p.Entries) != 2 {
		t.Fatal("unknown private payload shown")
	}
}
