package render

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

func TestProcessSelectionAndResizeNeverBroadcast(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m.input.SetValue("/terminal")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.panel == nil || m.pending != nil {
		t.Fatal("selection must be a read-only command")
	}
	m, _ = update(t, m, processesMsg{Generation: m.panel.generation, Page: process.ListPage{Processes: []process.State{{Spec: process.Spec{ID: "a", PTY: true}}, {Spec: process.Spec{ID: "b", PTY: true}}}}})
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.panel.id != "b" {
		t.Fatal("wrong selected target")
	}
	generation := m.panel.generation
	state := process.State{Spec: process.Spec{ID: "b", PTY: true}, Status: "running", Terminal: &process.Terminal{Cols: 80, Rows: 24}}
	m, _ = update(t, m, processLogMsg{Generation: generation, State: state, Output: process.Output{ProcessID: "b", Stream: "pty", Status: "running"}})
	m, cmd = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	if cmd == nil || m.resizePending == nil {
		t.Fatal("explicit target not resized")
	}
	var target process.ResizeParams
	if err := json.Unmarshal(m.resizePending.Params, &target); err != nil || target.ID != "b" || target.Cols != 60 || target.Rows != 20 {
		t.Fatal(target, err)
	}
	original := *m.resizePending
	m, cmd = update(t, m, tea.WindowSizeMsg{Width: 48, Height: 16})
	if cmd != nil || m.resizePending.OperationID != original.OperationID {
		t.Fatal("resize burst not coalesced")
	}
	state.Terminal = &process.Terminal{Cols: 60, Rows: 20}
	m, cmd = update(t, m, resizedMsg{Request: original, Result: process.ControlResult{State: state}})
	if cmd == nil || m.resizePending == nil || m.resizePending.OperationID == original.OperationID {
		t.Fatal("latest size lost")
	}
	json.Unmarshal(m.resizePending.Params, &target)
	if target.ID != "b" || target.Cols != 48 || target.Rows != 16 {
		t.Fatal(target)
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	_, cmd = update(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	if cmd != nil {
		t.Fatal("resize after target released")
	}
}

func TestReadOnlyLogsPipeAndUncertainResize(t *testing.T) {
	for _, pty := range []bool{true, false} {
		m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
		m.panel = &processPanel{kind: "log", id: "p", state: process.State{Spec: process.Spec{ID: "p", PTY: pty}, Status: "running", Terminal: &process.Terminal{Cols: 80, Rows: 24}}}
		m, cmd := update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
		if cmd != nil || m.resizePending != nil {
			t.Fatal("read-only observer resized process")
		}
		m.panel.control = true
		m, cmd = update(t, m, tea.WindowSizeMsg{Width: 48, Height: 16})
		if !pty {
			if cmd != nil {
				t.Fatal("pipe resize")
			}
			continue
		}
		original := *m.resizePending
		m, _ = update(t, m, resizedMsg{Request: original, Err: protocol.E("transport_unknown", "uncertain")})
		m, cmd = update(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
		if cmd != nil || m.resizePending.OperationID != original.OperationID {
			t.Fatal("unknown resize bypassed")
		}
		m.input.SetValue("/resize")
		m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil || m.resizePending.OperationID != original.OperationID || string(m.resizePending.Params) != string(original.Params) {
			t.Fatal("retry changed resize identity")
		}
		m, _ = update(t, m, resizedMsg{Request: original, Result: process.ControlResult{State: process.State{Spec: process.Spec{ID: "other"}}}})
		if m.resizePending == nil {
			t.Fatal("wrong acceptance cleared original resize")
		}
	}
}

func TestLogBytesAreBoundedLiteralAndKeepGaps(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m.panel = &processPanel{kind: "log", id: "p", generation: 1}
	data := append([]byte(strings.Repeat("x", 70000)), []byte("中文\x1b[2J\x1b]52;c;secret\a")...)
	m, _ = update(t, m, processLogMsg{Generation: 1, State: process.State{Spec: process.Spec{ID: "p"}, Status: "exited"}, Output: process.Output{ProcessID: "p", Stream: "stdout", Data: data, Next: int64(len(data)) + 10, Total: int64(len(data)) + 10, Gap: &process.Gap{From: int64(len(data)), To: int64(len(data)) + 10}}})
	if len(m.panel.data) > 64<<10 || !m.panel.truncated || m.panel.gap == nil {
		t.Fatal("missing bounds/gap")
	}
	view := strings.Join(m.panel.lines(48, 13, 0), "\n")
	if strings.Contains(view, "\x1b") || !strings.Contains(view, "missing bytes") {
		t.Fatal("control injection or hidden gap", view)
	}
	before := m.panel.offset
	m, _ = update(t, m, processLogMsg{Generation: 0, Output: process.Output{Data: []byte("stale")}})
	if m.panel.offset != before || strings.Contains(string(m.panel.data), "stale") {
		t.Fatal("obsolete read changed selection")
	}
}

func TestHandoffCancelsOnlySubscriptionAndReplaysOriginalCursor(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent"}, Cursor: 16}})
	old := m.generation
	for _, e := range historyFixture(t)[:10] {
		m, _ = update(t, m, watchEventMsg{e, old})
	}
	m.input.SetValue("/attach p")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || !m.attaching || m.watching || m.pending != nil || m.cursor != 10 {
		t.Fatal("handoff mutated core state")
	}
	m, _ = update(t, m, watchEventMsg{historyFixture(t)[10], old})
	if m.cursor != 10 {
		t.Fatal("old subscription altered handoff cursor")
	}
	m, cmd = update(t, m, attachmentFinishedMsg{})
	if cmd == nil || m.attaching || !m.watching || m.cursor != 10 {
		t.Fatal("return did not reattach at original cursor")
	}
}

func TestLogListSelectionStaysReadOnlyAcrossPages(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m.input.SetValue("/logs")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = update(t, m, processesMsg{Generation: m.panel.generation, Page: process.ListPage{Next: "p1"}})
	m, _ = update(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	m, _ = update(t, m, processesMsg{Generation: m.panel.generation, Page: process.ListPage{Processes: []process.State{{Spec: process.Spec{ID: "p2", PTY: true}}}}})
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.panel.id != "p2" || m.panel.control || m.resizePending != nil {
		t.Fatal("read-only list acquired PTY control")
	}
}

func TestProcessPanelFramesPreserveTargetAndFit(t *testing.T) {
	for _, kind := range []string{"list", "log"} {
		p := processPanel{kind: kind, id: "中文-target", data: []byte(strings.Repeat("中文é😀\n", 50)), truncated: true, gap: &process.Gap{From: 100, To: 200}, err: protocol.E("unavailable", "offline"), selection: 19}
		p.page.Next = "next"
		for range 20 {
			p.page.Processes = append(p.page.Processes, process.State{Spec: process.Spec{ID: "中文-process", Command: strings.Repeat("echo ", 30)}})
		}
		for _, width := range []int{80, 60, 48} {
			for _, rows := range []int{1, 2, 3, 13, 21} {
				lines := p.lines(width, rows, 0)
				if len(lines) > rows || len(lines) == 0 {
					t.Fatal(kind, width, rows, lines)
				}
				if kind == "log" && !strings.HasPrefix(lines[0], p.id) {
					t.Fatal("target heading was displaced", lines)
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > width {
						t.Fatal("frame overflow", kind, width, line)
					}
				}
			}
		}
	}
}
