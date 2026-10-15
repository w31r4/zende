package render

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func typeText(t *testing.T, m Shell, text string) Shell {
	t.Helper()
	for _, r := range text {
		m, _ = update(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func commandShell(t *testing.T) Shell {
	t.Helper()
	m := New(context.Background(), panicClient{}, "a", func(tea.Msg) {})
	m.loading, m.connected = false, true
	m.snapshot.Agent = protocol.Agent{ID: "a", Cwd: "/项目/é/🙂", Model: "fixture/local"}
	return m
}

func TestCommandRegistryOwnsHelpAliasesAndValidation(t *testing.T) {
	names := make(map[string]bool)
	for _, c := range commands {
		for _, name := range append([]string{c.name}, c.aliases...) {
			if names[name] || name == "" {
				t.Fatal("duplicate or empty command", name)
			}
			names[name] = true
			resolved, ok := lookupCommand(name)
			if !ok || resolved.name != c.name {
				t.Fatal("alias resolution", name)
			}
			m := commandShell(t)
			_, err := m.invoke(invocation{name: name})
			if err != nil && strings.Contains(err.Error(), "unknown") {
				t.Fatal("advertised command has no handler", name, err)
			}
			m = commandShell(t)
			m.input.SetValue("/" + name + strings.Repeat(" extra", c.maxArgs+1))
			before := m.input.Value()
			m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd != nil || m.pending != nil || m.attaching || m.panel != nil || m.input.Value() != before || m.err == nil {
				t.Fatal("invalid arguments caused an action or lost input", name)
			}
		}
	}
	m := commandShell(t)
	m.input.SetValue("/help")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.help || !strings.Contains(ansi.Strip(m.View().Content), "/help") {
		t.Fatal("registry help missing")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !strings.Contains(ansi.Strip(m.View().Content), "/detach") {
		t.Fatal("help cannot reach final command")
	}
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 48, Height: 16})
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !strings.Contains(ansi.Strip(m.View().Content), "/detach") {
		t.Fatal("narrow help cannot reach final command")
	}
}

func TestCommandQuotingIsLiteralAndRejectsIncompleteInput(t *testing.T) {
	call, err := parseInvocation(`/commentary 'output:request:中文 " $HOME $(cmd) :0'`)
	if err != nil || call.name != "commentary" || !reflect.DeepEqual(call.args, []string{`output:request:中文 " $HOME $(cmd) :0`}) {
		t.Fatal(call, err)
	}
	for _, text := range []string{`/logs "unfinished`, `/logs trailing\`, `/logs 'unfinished`} {
		m := commandShell(t)
		m.input.SetValue(text)
		m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd != nil || m.panel != nil || m.input.Value() != text || m.err == nil {
			t.Fatal("invalid quoting changed state", text)
		}
	}
}

func TestPaletteBelowComposerAndStatusStaysLast(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := commandShell(t)
			m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.report(protocol.E("test", strings.Repeat("警告 ", 80)))
			m = typeText(t, m, "/")
			f, v := m.layout(), m.View()
			lines := strings.Split(ansi.Strip(v.Content), "\n")
			if len(lines) != size[1] || len(f.candidates) != 4 || v.Cursor == nil || v.Cursor.Y != len(f.body) || !v.AltScreen || v.MouseMode != tea.MouseModeCellMotion {
				t.Fatal("candidate/input layout", len(lines), v.Cursor)
			}
			for y, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("overflow", y, line)
				}
			}
			for i, line := range f.candidates {
				if strings.TrimRight(lines[len(f.body)+2+i], " ") != line {
					t.Fatal("candidate covered composer", i)
				}
			}
			if strings.TrimRight(lines[size[1]-1], " ") != f.status {
				t.Fatal("status displaced")
			}
			for range len(commands) - 1 {
				m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
			}
			if m.palette.items[m.palette.index].name != "detach" || !strings.Contains(strings.Join(m.layout().candidates, "\n"), "/detach") {
				t.Fatal("candidate list cannot reach last entry")
			}
		})
	}
}

func TestInlineCommandAndTabCompletionPreserveDraft(t *testing.T) {
	const draft = "中文 é 🙂\nsecond line "
	m := commandShell(t)
	m, _ = update(t, m, tea.PasteMsg{Content: draft})
	m = typeText(t, m, "/he")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.help || m.input.Value() != draft || m.commandDraft != nil || m.pending != nil {
		t.Fatal("local command lost or submitted draft", m.input.Value())
	}
	m = typeText(t, m, "/back")
	if m.palette.items[m.palette.index].name != "back" {
		t.Fatal("exact command lost to longer prefix")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.help || m.showActivity || m.input.Value() != draft {
		t.Fatal("return did not preserve draft")
	}
	m = typeText(t, m, "/ter")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.input.Value() != "/terminal " || m.commandDraft == nil || m.commandDraft.Value() != draft || m.panel != nil {
		t.Fatal("Tab executed instead of completing", m.input.Value())
	}
	m = typeText(t, m, "process-1")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.panel == nil || m.panel.id != "process-1" || m.input.Value() != draft {
		t.Fatal("command arguments lost", m.panel, m.input.Value())
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.panel != nil || m.input.Value() != draft {
		t.Fatal("panel exit lost draft")
	}
	m = typeText(t, m, "/resi")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.err == nil || m.commandDraft == nil || m.input.Value() != "/resize" {
		t.Fatal("rejected command cannot be corrected")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.commandDraft != nil || m.input.Value() != draft {
		t.Fatal("cancel lost held draft")
	}
}

func TestPasteIMEAndCaretDoNotExecuteCandidates(t *testing.T) {
	m := commandShell(t)
	m = typeText(t, m, "/")
	m, cmd := update(t, m, tea.PasteMsg{Content: "help\n中文"})
	if cmd != nil || m.paletteActive() || m.help || m.pending != nil || m.input.Value() != "/help\n中文" {
		t.Fatal("paste treated as command selection")
	}
	m = commandShell(t)
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "中文输入"})
	if m.pending != nil || m.input.Value() != "中文输入" {
		t.Fatal("committed IME text was submitted", cmd)
	}
	m = typeText(t, m, " /he")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.paletteActive() {
		t.Fatal("candidate editing away from token end")
	}
	for _, literal := range []string{"see /tmp/file", "/logs /temporary-path", "use $skill-later"} {
		m = typeText(t, commandShell(t), literal)
		if m.paletteActive() || m.pending != nil {
			t.Fatal("argument/path or unavailable skill became a command", literal)
		}
	}
}

func TestControlCommandAcceptanceNeverClearsRestoredDraft(t *testing.T) {
	m := typeText(t, commandShell(t), "pending draft /stop")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.pending == nil || m.pending.Method != "agent.stop" || m.input.Value() != "pending draft " {
		t.Fatal("control command changed draft or target")
	}
	original := *m.pending
	m, _ = update(t, m, acceptedMsg{Request: original, Err: protocol.E("transport_unknown", "uncertain")})
	m = typeText(t, m, "/retry")
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.pending.OperationID != original.OperationID || m.input.Value() != "pending draft " {
		t.Fatal("retry changed identity or draft")
	}
	m, _ = update(t, m, acceptedMsg{Request: original, Accepted: protocol.Accepted{AgentID: "a", OperationID: original.OperationID}})
	if m.pending != nil || m.input.Value() != "pending draft " {
		t.Fatal("acceptance erased restored draft")
	}
}

func TestEveryClickTargetHasRegistryRouteAndKeepsDraft(t *testing.T) {
	makeModel := func() Shell {
		m := commandShell(t)
		m.connected = false
		m.activity.Items = []protocol.ActivityItem{{Kind: "task", ID: "task", State: "running"}}
		m.report(protocol.E("test", "warning"))
		m.issues.open = false
		m.transcript.Entries = []Entry{{Key: "output:request:message:0", Kind: "assistant", Category: "commentary", State: "confirmed", Text: "过程说明"}}
		m.input.SetValue("independent 中文 draft")
		return m
	}
	base := makeModel()
	for _, target := range base.layout().targets {
		if _, ok := lookupCommand(target.call.name); !ok || target.candidate >= 0 {
			t.Fatal("unreachable click target", target)
		}
		mouse, _ := update(t, makeModel(), tea.MouseClickMsg{X: target.x, Y: target.y, Button: tea.MouseLeft})
		typed := makeModel()
		call := "/" + target.call.name
		for _, arg := range target.call.args {
			call += " '" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
		draft := NewComposer()
		draft.SetValue(typed.input.Value())
		typed.commandDraft = &draft
		typed.input.SetValue(call)
		typed, _ = update(t, typed, tea.KeyPressMsg{Code: tea.KeyEnter})
		if mouse.showActivity != typed.showActivity || mouse.issues.open != typed.issues.open || mouse.refreshing != typed.refreshing || !reflect.DeepEqual(mouse.commentaryOverrides, typed.commentaryOverrides) || mouse.input.Value() != base.input.Value() || typed.input.Value() != base.input.Value() {
			t.Fatal("click and command differ", target.call)
		}
		if mouse.pending != nil || typed.pending != nil {
			t.Fatal("read-only UI target submitted work")
		}
	}
	m := makeModel()
	m.input.Reset()
	m = typeText(t, m, "/he")
	for _, target := range m.layout().targets {
		if target.candidate >= 0 {
			m, _ = update(t, m, tea.MouseClickMsg{X: target.x, Y: target.y, Button: tea.MouseLeft})
			if !m.help || m.input.Value() != "" || m.pending != nil {
				t.Fatal("candidate click bypassed dispatcher")
			}
			break
		}
	}
}

func TestCommentaryHitIdentitySurvivesWrapScrollAndFold(t *testing.T) {
	m := commandShell(t)
	m.width, m.height = 48, 16
	m.input.SetWidth(48)
	m.input.SetValue("draft")
	m.transcript.Entries = []Entry{
		{Key: "first", Kind: "assistant", Category: "commentary", State: "confirmed", Text: strings.Repeat("中文é🙂 ", 120)},
		{Key: "hidden", Kind: "assistant", Category: "commentary", Text: "hidden", Hidden: true},
		{Key: "second", Kind: "assistant", Category: "commentary", State: "confirmed", Text: "second"},
		{Key: "final", Kind: "assistant", Category: "final", State: "confirmed", Text: "answer"},
	}
	before := append([]Entry(nil), m.transcript.Entries...)
	m.scroll = 4
	f := m.layout()
	var target *hitTarget
	for _, hit := range f.targets {
		if hit.call.name == "commentary" && hit.call.args[0] == "first" {
			copy := hit
			target = &copy
			break
		}
	}
	if target == nil {
		t.Fatal("wrapped commentary lost identity")
	}
	m, _ = update(t, m, tea.MouseClickMsg{X: target.x, Y: target.y, Button: tea.MouseLeft})
	if !m.commentaryOverrides["first"] || m.commentaryOverrides["second"] || !reflect.DeepEqual(before, m.transcript.Entries) || m.input.Value() != "draft" {
		t.Fatal("fold mutated history, wrong entry or draft")
	}
	for _, hit := range m.layout().targets {
		if hit.call.name == "commentary" && hit.call.args[0] == "first" {
			m, _ = update(t, m, tea.MouseClickMsg{X: hit.x, Y: hit.y, Button: tea.MouseLeft})
			if m.commentaryOverrides["first"] {
				t.Fatal("folded row cannot expand")
			}
			break
		}
	}
	m.transcript.Entries = m.transcript.Entries[2:]
	if m.toggleCommentary("first") || len(m.commentaryOverrides) != 0 {
		t.Fatal("expired entry retained override")
	}
	for _, msg := range []tea.Msg{tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft}, tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseRight}, tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft, Mod: tea.ModShift}, tea.MouseClickMsg{X: -1, Y: 0, Button: tea.MouseLeft}} {
		m, _ = update(t, m, msg)
		if len(m.commentaryOverrides) != 0 || m.input.Value() != "draft" {
			t.Fatal("non-action mouse event changed UI")
		}
	}
}

func TestCommentaryEntrySearchProvidesKeyboardAccessWithoutOpaqueIDs(t *testing.T) {
	m := commandShell(t)
	m.transcript.Entries = []Entry{
		{Key: "output:request:provider ' id :0", Kind: "assistant", Category: "commentary", State: "confirmed", Text: "正在检查配置"},
		{Key: "output:request:second:0", Kind: "assistant", Category: "commentary", State: "confirmed", Text: "正在运行测试"},
	}
	m = typeText(t, m, "主草稿 /comm")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.paletteActive() || len(m.palette.items) != 2 || m.commandDraft == nil {
		t.Fatal("per-entry keyboard candidates missing")
	}
	m = typeText(t, m, "配置")
	if len(m.palette.items) != 1 || !strings.Contains(m.layout().candidates[0], "正在检查配置") {
		t.Fatal("commentary search missing")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.paletteActive() {
		t.Fatal("Tab reselected a completed argument")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.commentaryOverrides[m.transcript.Entries[0].Key] || m.commentaryOverrides[m.transcript.Entries[1].Key] || m.input.Value() != "主草稿 " {
		t.Fatal("keyboard entry action differs from click or lost draft")
	}
}
