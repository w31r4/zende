package render

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type panicClient struct{}

func (panicClient) Retry(context.Context, protocol.Request, any, int, time.Duration) error {
	panic("I/O in Update/View")
}
func (panicClient) Watch(context.Context, protocol.Cursor, func(protocol.Event) error) error {
	panic("I/O in Update/View")
}

func update(t *testing.T, m Shell, msg tea.Msg) (Shell, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(Shell), cmd
}

func TestUpdateAndViewNeverRunCoreIO(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) { panic("send in Update") })
	if m.Init() == nil {
		t.Fatal("missing initial command")
	}
	var cmd tea.Cmd
	m, cmd = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent", Cwd: "/project"}, Cursor: 0}})
	if cmd == nil || !m.watching || m.cursor != 0 {
		t.Fatal("watch must start at the projection cursor")
	}
	m, cmd = update(t, m, coreEventMsg{protocol.Event{Version: 1, AgentID: "agent", Seq: 1, Kind: "task.running"}})
	if cmd == nil || !m.refreshing {
		t.Fatal("missing asynchronous refresh")
	}
	m, cmd = update(t, m, coreEventMsg{protocol.Event{Version: 1, AgentID: "agent", Seq: 2, TaskID: "task", Kind: "task.completed", Payload: json.RawMessage(`{"state":"completed"}`)}})
	if cmd != nil || !m.refreshAgain {
		t.Fatal("unbounded refreshes")
	}
	m, _ = update(t, m, tea.PasteMsg{Content: "中文\n<final>literal</final>"})
	if m.input.Value() != "中文\n<final>literal</final>" || m.pending != nil {
		t.Fatal("paste submitted", m.input.Value())
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.pending == nil || m.pending.Method != "agent.submit" {
		t.Fatal("missing submit command")
	}
	var input protocol.Submit
	if json.Unmarshal(m.pending.Params, &input) != nil || input.Input != m.input.Value() {
		t.Fatal(input)
	}
	if !m.View().AltScreen {
		t.Fatal("not alt screen")
	}
	_, cmd = update(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C must detach")
	}
}

func TestPendingMutationRetainsIdentityAndNewDraft(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m.input.SetValue("original")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	original := *m.pending
	m, _ = update(t, m, acceptedMsg{Request: original, Err: protocol.E("transport_unknown", "query original operation")})
	m.input.SetValue("another draft")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || m.pending.OperationID != original.OperationID {
		t.Fatal("unknown mutation bypassed")
	}
	m.input.SetValue("/retry")
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.pending.OperationID != original.OperationID || string(m.pending.Params) != string(original.Params) {
		t.Fatal("retry changed identity")
	}
	m.input.SetValue("another draft")
	m, _ = update(t, m, acceptedMsg{Request: original, Accepted: protocol.Accepted{OperationID: original.OperationID, AgentID: "agent"}})
	if m.pending != nil || m.input.Value() != "another draft" {
		t.Fatal("accepted callback destroyed newer input")
	}
}

func TestShellSizesAndLiteralControlText(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 24}, {48, 16}, {20, 8}} {
		m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
		m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.snapshot = protocol.Snapshot{Agent: protocol.Agent{ID: "agent", Cwd: "/路径/é/🙂"}, Tasks: []protocol.Task{{Input: "input", Result: "中文 é 🙂 \x1b[2J\x1b]52;c;secret\a"}}}
		m.transcript.Entries = []Entry{{Kind: "assistant", Category: "final", State: "confirmed", Text: m.snapshot.Tasks[0].Result}}
		m.input.SetValue("中文 é 🙂")
		view := m.View()
		if !view.AltScreen || strings.Contains(view.Content, "\x1b[2J") || strings.Contains(view.Content, "\x1b]52;") {
			t.Fatal("control injection", view.Content)
		}
		lines := strings.Split(view.Content, "\n")
		if len(lines) > size[1] {
			t.Fatal(size, "height", len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal(size, "overflow", ansi.StringWidth(line))
			}
		}
		if size[0] >= 48 && (view.Cursor == nil || view.Cursor.Y >= size[1]-1 || view.Cursor.X >= size[0]) {
			t.Fatal(size, "lost input cursor", view.Cursor)
		}
	}
}

func TestDetachAndCommandsDoNotStopWorkImplicitly(t *testing.T) {
	for _, command := range []string{"/quit", "/detach", "/stop", "/resume", "/recover", "/help"} {
		m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
		m.input.SetValue(command)
		m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		switch command {
		case "/quit", "/detach":
			if _, ok := cmd().(tea.QuitMsg); !ok || m.pending != nil {
				t.Fatal(command)
			}
		case "/help":
			if cmd != nil || !m.help || m.pending != nil {
				t.Fatal(command)
			}
		default:
			if cmd == nil || m.pending == nil || m.pending.Method != "agent."+command[1:] {
				t.Fatal(command)
			}
		}
	}
}
