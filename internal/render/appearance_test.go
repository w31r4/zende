package render

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func TestMinimalFramesThemeAndSymbolFallback(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256, colorprofile.ANSI, colorprofile.ASCII} {
			for _, ascii := range []bool{false, true} {
				t.Run(fmt.Sprintf("%dx%d/%s/ascii=%t", size[0], size[1], profile, ascii), func(t *testing.T) {
					m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
					m.theme.ascii = ascii
					m, _ = update(t, m, tea.ColorProfileMsg{Profile: profile})
					m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					m.loading, m.connected = false, true
					m.snapshot.Agent = protocol.Agent{ID: "a", Cwd: "/项目", Model: "endpoint/fixture"}
					m.transcript.Entries = []Entry{{Kind: "user", Text: "调整输入区。"}, {Kind: "assistant", Category: "commentary", State: "confirmed", Text: "正在处理。"}, {Kind: "assistant", Category: "final", State: "confirmed", Text: "完成。"}}
					m.input.SetValue("中文 é 🙂")
					v := m.View()
					plain := ansi.Strip(v.Content)
					lines := strings.Split(plain, "\n")
					if len(lines) != size[1] {
						t.Fatal("height", len(lines))
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > size[0] {
							t.Fatal("width", line)
						}
					}
					want := map[int]string{0: "> 调整输入区。", 1: "~ 正在处理。", 2: "= 完成。", size[1] - 3: "> 中文 é 🙂", size[1] - 2: "", size[1] - 1: m.theme.modelMark() + " fixture" + m.theme.separator() + "/项目"}
					for row, line := range lines {
						if strings.TrimRight(line, " ") != want[row] {
							t.Fatalf("row %d: %q want %q", row, line, want[row])
						}
					}
					if strings.Contains(plain, "send") || strings.Contains(plain, "bg") || strings.Contains(plain, "! ") {
						t.Fatal("idle chrome", plain)
					}
					if profile >= colorprofile.ANSI256 {
						if !strings.Contains(v.Content, "38;2;59;225;59") || strings.Contains(v.Content, "38;2;255;255;255") {
							t.Fatal("foreground is not user's green")
						}
					} else if strings.Contains(v.Content, "38;") || strings.Contains(v.Content, "48;") {
						t.Fatal("low color approximated custom ANSI slots")
					}
					if profile == colorprofile.ASCII && strings.Contains(v.Content, "\x1b") {
						t.Fatal("monochrome frame has style escapes")
					}
					if v.Cursor == nil || v.Cursor.Y != size[1]-3 {
						t.Fatal("cursor left input", v.Cursor)
					}
				})
			}
		}
	}
}

func TestIssuesFirstOccurrenceOnlyAndBoundedMemory(t *testing.T) {
	m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
	m.loading, m.connected = false, true
	raise := func(command string) {
		m.input.SetValue(command)
		m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	raise("/not-a-command")
	if !m.issues.open || !strings.Contains(ansi.Strip(m.View().Content), "unknown command") {
		t.Fatal("first error hidden")
	}
	raise("/still-not-a-command")
	if m.issues.open || strings.Contains(ansi.Strip(m.View().Content), "unknown command") || !strings.Contains(m.statusLine(), "! 1") {
		t.Fatal("repeat not folded")
	}
	raise("/issues")
	if !m.issues.open || !strings.Contains(ansi.Strip(m.View().Content), "x2") {
		t.Fatal("detail not reachable")
	}
	raise("/another-invalid-command")
	if !m.issues.open {
		t.Fatal("repeated polling/error closed explicitly opened detail")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.issues.open {
		t.Fatal("escape did not fold")
	}
	m.report(nil)
	if len(m.issues.active) != 0 || strings.Contains(m.statusLine(), "! ") {
		t.Fatal("resolved issue still active")
	}
	m.report(protocol.E("invalid_params", "again"))
	if m.issues.open {
		t.Fatal("resolved class forgotten immediately")
	}
	for i := range 100 {
		m.report(protocol.E(fmt.Sprint("kind", i), strings.Repeat("x", 3000)))
	}
	if len(m.issues.seen) > 64 || len(m.issues.active) != 1 || len(m.issues.active[0].text) > 520 {
		t.Fatal("unbounded error cache")
	}
}

func TestActivityCountIsConditionalAndPollingIsCoalesced(t *testing.T) {
	m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
	m.loading, m.connected = false, true
	m.theme.ascii = true
	if m.activityLabel() != "" {
		t.Fatal("empty background entry")
	}
	if m.startActivity() == nil || m.startActivity() != nil {
		t.Fatal("unbounded polling")
	}
	gen := m.activityGeneration
	m, _ = update(t, m, activityMsg{Generation: gen, Activity: protocol.Activity{AgentID: "a", Items: []protocol.ActivityItem{{Kind: "task", ID: "t", AgentID: "a", State: "running"}}}})
	if m.activityLabel() != "bg 1" {
		t.Fatal(m.activityLabel())
	}
	m.input.SetValue("/background")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.showActivity || m.pending != nil || !strings.Contains(ansi.Strip(m.View().Content), "task t | running") {
		t.Fatal("background command")
	}
	m, cmd := update(t, m, activityTickMsg{gen})
	if cmd != nil {
		t.Fatal("obsolete timer started another read")
	}
	m, _ = update(t, m, activityMsg{Generation: m.activityGeneration, Err: protocol.E("unavailable", "offline")})
	if m.activityLabel() != "bg 1?" {
		t.Fatal("offline became zero", m.activityLabel())
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.showActivity {
		t.Fatal("background did not close")
	}
	m, _ = update(t, m, activityTickMsg{m.activityGeneration})
	if !m.activityWaiting {
		t.Fatal("tick lost state transition")
	}
}

func TestComposerDraftsAndPasteAreIndependent(t *testing.T) {
	main, question := NewComposer(), NewComposer()
	main.SetValue("主输入草稿")
	question, _ = question.Update(tea.PasteMsg{Content: "回答\n中文 é 🙂"})
	if main.Value() != "主输入草稿" || question.Value() != "回答\n中文 é 🙂" {
		t.Fatal("draft mixing")
	}
	question.setTheme(theme{profile: colorprofile.ASCII})
	question.SetWidth(48)
	if question.Value() != "回答\n中文 é 🙂" {
		t.Fatal("theme/resize erased draft")
	}
}

func TestBusyNarrowFrameKeepsInputStatusAndErrorBudget(t *testing.T) {
	for _, width := range []int{80, 60, 48} {
		m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
		m, _ = update(t, m, tea.WindowSizeMsg{Width: width, Height: 16})
		m.snapshot.Agent = protocol.Agent{ID: "a", Cwd: "/很长的路径/" + strings.Repeat("嵌套/", 40) + "project", Model: "endpoint/" + strings.Repeat("model", 40)}
		m.loading, m.connected = false, false
		m.activity = protocol.Activity{AgentID: "a", Items: make([]protocol.ActivityItem, 1000), Truncated: true, Unavailable: []string{"processes"}}
		m.input.SetValue("草稿")
		m.report(protocol.E("invalid_state", strings.Repeat("长错误", 200)))
		m.transcript.Entries = []Entry{{Kind: "assistant", Category: "commentary", State: "confirmed", Text: strings.Repeat("过程\n", 100)}}
		m.foldCommentary = true
		lines := strings.Split(ansi.Strip(m.View().Content), "\n")
		if len(lines) != 16 || !strings.Contains(lines[13], "草稿") || !strings.Contains(lines[15], "! 1") || !strings.Contains(lines[15], "offline") || !strings.Contains(lines[15], "1000+?") {
			t.Fatal(width, lines)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatal("overflow", line)
			}
		}
		if !strings.Contains(lines[0], "~ ...") || m.transcript.Entries[0].Text == "..." {
			t.Fatal("fold deleted source")
		}
		if strings.TrimSpace(lines[15]) == "" {
			t.Fatal("status lost")
		}
	}
}

func TestDefaultModelStatusComesFromSavedPublicBinding(t *testing.T) {
	m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
	m.loading, m.connected = false, true
	m.snapshot.Agent.Cwd = "/project"
	m, _ = update(t, m, coreEventMsg{protocol.Event{Version: 1, AgentID: "a", Seq: 1, Kind: "task.config", Payload: []byte(`{"model":"actual-model","binding":{"credential":"DO_NOT_DISPLAY"}}`)}})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(m.statusLine(), "actual-model") || strings.Contains(view, "DO_NOT_DISPLAY") {
		t.Fatal("effective model lost or private data exposed")
	}
}

func TestActivityNarrowRowsKeepStateAndOriginalIdentity(t *testing.T) {
	m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
	id := "task_" + strings.Repeat("a", 40)
	m.activity.Items = []protocol.ActivityItem{{Kind: "task", ID: id, AgentID: "a", State: "cancelling"}}
	lines := m.activityLines(48, 10)
	if len(lines) != 2 || !strings.HasSuffix(lines[1], "cancelling") || ansi.StringWidth(lines[1]) > 48 || m.activity.Items[0].ID != id {
		t.Fatal(lines)
	}
}
