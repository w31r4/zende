package render

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func selectionShell(t *testing.T) (Shell, *imageTestClient) {
	t.Helper()
	m := commandShell(t)
	m.snapshot.Agent.Cwd = "/project"
	client := &imageTestClient{}
	client.handle = func(req protocol.Request, target any) error {
		if req.OperationID != "" {
			t.Fatal("browsing made a mutation", req.Method)
		}
		switch req.Method {
		case "agent.selection":
			*target.(*protocol.AgentSelection) = protocol.AgentSelection{AgentID: "a", Cwd: "/project", Revision: 7}
		case "model.connections":
			*target.(*protocol.ConnectionPage) = protocol.ConnectionPage{Version: "catalog-1", Items: []protocol.ConnectionInfo{
				{ID: "one", Label: "Same brand", Protocol: "responses", Supported: true},
				{ID: "two", Label: "Same brand", Protocol: "responses", Supported: true},
			}}
		case "model.list":
			var q protocol.ModelQuery
			if protocol.StrictDecode(req.Params, &q) != nil || q.Version != "catalog-1" || q.Limit != 40 {
				t.Fatal(q)
			}
			*target.(*protocol.ModelPage) = protocol.ModelPage{Connection: protocol.ConnectionInfo{ID: q.Connection, Supported: true}, Version: q.Version,
				Items: []protocol.ModelInfo{{ID: "configured", Efforts: []string{"low", "high"}}}}
		case "workspace.directory":
			var q protocol.DirectoryQuery
			if protocol.StrictDecode(req.Params, &q) != nil || q.AgentID != "a" || q.Limit != 40 || !filepath.IsAbs(q.Path) {
				t.Fatal(q)
			}
			p := protocol.DirectoryPage{Path: filepath.Clean(q.Path), Entries: []protocol.DirectoryEntry{{Name: "中文 folder"}, {Name: "z-last"}}}
			if p.Path != "/" {
				p.Parent = filepath.Dir(p.Path)
			}
			*target.(*protocol.DirectoryPage) = p
		default:
			t.Fatal("unexpected query", req.Method)
		}
		return nil
	}
	m.client = client
	return m, client
}

func selectionRead(t *testing.T, m Shell, name string, args ...string) Shell {
	t.Helper()
	cmd, err := m.invoke(invocation{name, args})
	if err != nil || cmd == nil {
		t.Fatal(name, err, "missing read")
	}
	m, _ = update(t, m, cmd())
	if m.selection == nil || !m.selection.ready || m.selection.waiting {
		t.Fatal("selection query failed", m.issues.active)
	}
	return m
}

func selectionLocal(t *testing.T, m Shell, name string, args ...string) Shell {
	t.Helper()
	cmd, err := m.invoke(invocation{name, args})
	if err != nil || cmd != nil || m.pending != nil {
		t.Fatal(name, err, "local choice caused I/O")
	}
	return m
}

func TestModelPickerUsesConfiguredConnectionsAndDefersAllWrites(t *testing.T) {
	for _, stage := range []string{"connections", "models", "efforts", "confirm"} {
		t.Run(stage, func(t *testing.T) {
			m, _ := selectionShell(t)
			m.input.SetValue("中文 draft")
			addDraftImage(t, &m.input, imageInfo("image"))
			addDraftSkill(t, &m.input, uiSkill("skill"))
			stamp := m.input.stamp()
			m = selectionRead(t, m, "model")
			view := ansi.Strip(m.View().Content)
			if !strings.Contains(view, "Same brand · one") || !strings.Contains(view, "Same brand · two") || strings.Contains(view, "OpenAI") {
				t.Fatal("catalog is fabricated or endpoint identity was lost", view)
			}
			if stage != "connections" {
				m = selectionRead(t, m, "model", "two")
			}
			if stage == "efforts" || stage == "confirm" {
				m = selectionLocal(t, m, "model", "two", "configured")
			}
			if stage == "confirm" {
				m = selectionLocal(t, m, "effort", "high")
			}
			if m.selection.kind != stage || m.pending != nil {
				t.Fatal("selection skipped a stage or committed early")
			}
			m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.selection != nil || m.pending != nil || m.input.Value() != "中文 draft" || m.input.stamp() != stamp || len(m.input.images) != 1 || len(m.input.skills) != 1 {
				t.Fatal("cancellation mutated settings or draft")
			}
		})
	}
	m, _ := selectionShell(t)
	m = selectionRead(t, m, "model")
	m = selectionRead(t, m, "model", "two")
	m = selectionLocal(t, m, "model", "two", "configured")
	if _, err := m.invoke(invocation{"effort", []string{"unconfigured"}}); err == nil || m.selection.kind != "efforts" {
		t.Fatal("invented effort accepted")
	}
	m = selectionLocal(t, m, "effort", "high")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.pending != nil {
		t.Fatal("traversal confirmed a mutation")
	}
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	var q protocol.SelectAgent
	if cmd == nil || m.pending == nil || m.pending.Method != "agent.select" || protocol.StrictDecode(m.pending.Params, &q) != nil ||
		q.Model == nil || *q.Model != "two/configured" || q.Effort == nil || *q.Effort != "high" || q.CatalogVersion != "catalog-1" ||
		q.ExpectedRevision == nil || *q.ExpectedRevision != 7 || q.Cwd != nil {
		t.Fatal("unfenced or incorrect selection", q)
	}
}

func TestSelectionEmptyStaleAndUnavailableCatalogs(t *testing.T) {
	m, client := selectionShell(t)
	handle := client.handle
	client.handle = func(req protocol.Request, out any) error {
		if req.Method == "model.connections" {
			*out.(*protocol.ConnectionPage) = protocol.ConnectionPage{Version: "empty"}
			return nil
		}
		return handle(req, out)
	}
	m = selectionRead(t, m, "model")
	if !strings.Contains(ansi.Strip(m.View().Content), "No configured connections") {
		t.Fatal("empty configuration hidden")
	}
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending != nil || cmd != nil {
		t.Fatal("empty catalog submitted")
	}
	client.handle = handle
	m = selectionRead(t, m, "model")
	m.selection.connections.Items[0].Supported = false
	if _, err := m.invoke(invocation{"model", []string{"one"}}); err == nil {
		t.Fatal("unavailable adapter selected")
	}
	cmd, err := m.invoke(invocation{"model", []string{"two"}})
	if err != nil {
		t.Fatal(err)
	}
	stale := cmd().(selectionMsg)
	stale.models.Version = "new-version"
	m, _ = update(t, m, stale)
	if m.selection.ready || m.selection.waiting || len(m.issues.active) != 1 {
		t.Fatal("catalog mismatch was accepted")
	}
	cmd, _ = m.invoke(invocation{name: "model"})
	stale = cmd().(selectionMsg)
	m.closePanel()
	m, _ = update(t, m, stale)
	if m.selection != nil {
		t.Fatal("late response reopened cancelled picker")
	}
}

func TestDirectoryNavigationDotPathAndPaginationKeepRevision(t *testing.T) {
	m, client := selectionShell(t)
	m.input.SetValue("kept draft")
	m = selectionRead(t, m, "cd")
	_, choices := m.selectionChoices()
	if choices[0].text != "." || choices[1].text != ".." || choices[0].call.name != "select" {
		t.Fatal(choices)
	}
	for range 2 {
		m, _ = update(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	m, cmd := update(t, m, tea.KeyPressMsg{Code: 'l', Text: "l"})
	if cmd == nil || m.pending != nil {
		t.Fatal("directory traversal committed")
	}
	m, _ = update(t, m, cmd())
	if m.selection.directory.Path != "/project/中文 folder" || m.snapshot.Agent.Cwd != "/project" || m.input.Value() != "kept draft" {
		t.Fatal("browse changed cwd or draft")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: 'h', Text: "h"})
	m, _ = update(t, m, cmd())
	if m.selection.directory.Path != "/project" {
		t.Fatal("h did not navigate parent")
	}
	m = selectionRead(t, m, "cd", "/")
	_, choices = m.selectionChoices()
	if choices[0].text != "." || choices[1].text == ".." {
		t.Fatal("root has a false parent")
	}
	// Literal absolute paths, spaces and shell syntax go through the composer.
	m = typeText(t, m, "/cd '/project/space $HOME'")
	if m.commandDraft == nil || m.commandDraft.Value() != "kept draft" {
		t.Fatal("path entry lost original draft")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = update(t, m, cmd())
	if m.input.Value() != "kept draft" || m.selection.directory.Path != "/project/space $HOME" {
		t.Fatal("path was expanded or draft consumed")
	}
	m.selection.directory.Next = "page-last"
	cmd, err := m.selectionNext()
	if err != nil {
		t.Fatal(err)
	}
	m, _ = update(t, m, cmd())
	var query protocol.DirectoryQuery
	if json.Unmarshal(client.requests[len(client.requests)-1].Params, &query) != nil || query.After != "page-last" || query.Path != "/project/space $HOME" || m.selection.base.Revision != 7 {
		t.Fatal("pagination changed identity", query)
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	var choice protocol.SelectAgent
	if cmd == nil || m.pending == nil || protocol.StrictDecode(m.pending.Params, &choice) != nil ||
		choice.Cwd == nil || *choice.Cwd != query.Path || choice.Model != nil || choice.ExpectedRevision == nil || *choice.ExpectedRevision != 7 {
		t.Fatal("dot did not confirm exactly the browsed directory", choice)
	}
}

func TestSelectionAcceptanceFencingRetryAndDraft(t *testing.T) {
	m, _ := selectionShell(t)
	m.input.SetValue("never consume")
	m = selectionRead(t, m, "cd")
	cmd := m.dispatch(invocation{name: "select"})
	if cmd == nil {
		t.Fatal("missing mutation")
	}
	req := *m.pending
	m, _ = update(t, m, acceptedMsg{Request: req, Err: protocol.E("transport_unknown", "lost response")})
	if m.pending == nil || m.selection == nil {
		t.Fatal("uncertain acceptance lost request")
	}
	if _, err := m.invoke(invocation{name: "select"}); err == nil || m.pending.OperationID != req.OperationID {
		t.Fatal("unresolved request replaced")
	}
	m, _ = update(t, m, acceptedMsg{Request: req, Accepted: protocol.Accepted{OperationID: req.OperationID, AgentID: "a"}})
	if m.pending == nil {
		t.Fatal("missing selection receipt accepted")
	}
	m.git = protocol.GitStatus{Path: "/project", State: "ready", Branch: "old"}
	m.gitWaiting, m.gitPath, m.gitGeneration = true, "/project", 1
	choice := protocol.AgentSelection{AgentID: "a", Cwd: "/new", Revision: 8}
	m, _ = update(t, m, acceptedMsg{Request: req, Accepted: protocol.Accepted{OperationID: req.OperationID, AgentID: "a", Selection: &choice}})
	if m.pending != nil || m.selection != nil || m.input.Value() != "never consume" || m.git.State != "" || m.gitWaiting {
		t.Fatal("acceptance did not close picker or invalidate old Git")
	}
	m, _ = update(t, m, gitMsg{generation: 1, status: protocol.GitStatus{Path: "/project", State: "ready", Branch: "old"}})
	if m.git.State != "" {
		t.Fatal("late query restored old branch")
	}
	// A stale expected revision is a definite rejection, never a success.
	m, _ = selectionShell(t)
	m = selectionRead(t, m, "cd")
	m.dispatch(invocation{name: "select"})
	req = *m.pending
	m, _ = update(t, m, acceptedMsg{Request: req, Err: protocol.E("selection_changed", "reopen selection")})
	if m.pending != nil || m.selection == nil || m.snapshot.Agent.Cwd != "/project" {
		t.Fatal("rejected choice changed state")
	}
}

func TestGitRefreshCoalescesAndClearsOldOrUnknownState(t *testing.T) {
	m, _ := selectionShell(t)
	cmd := m.syncGit(false)
	generation := m.gitGeneration
	if cmd == nil || m.syncGit(true) != nil {
		t.Fatal("Git queries are not coalesced")
	}
	m, tick := update(t, m, gitMsg{generation: generation, status: protocol.GitStatus{Path: "/project", State: "ready", Branch: "main", Untracked: 1, Upstream: "origin/main"}})
	if tick == nil || m.gitLabel() != "main*" || !strings.Contains(strings.Join(m.gitLines(), "\n"), "+? -?") {
		t.Fatal("unknown upstream counts became exact zero")
	}
	m.snapshot.Agent.Cwd = "/other"
	if m.syncGit(false) == nil || m.gitLabel() != "" || m.git.State != "" {
		t.Fatal("cwd change retained old branch")
	}
	m, _ = update(t, m, gitMsg{generation: generation, status: protocol.GitStatus{Path: "/project", State: "ready", Branch: "stale"}})
	if m.git.State != "" {
		t.Fatal("stale Git result applied")
	}
	m, _ = update(t, m, gitMsg{generation: m.gitGeneration, status: protocol.GitStatus{Path: "/other", State: "none"}})
	if m.gitLabel() != "" || !strings.Contains(strings.Join(m.gitLines(), "\n"), "Not a Git repository") {
		t.Fatal("non-Git directory retained branch")
	}
	m.syncGit(true)
	m, _ = update(t, m, gitMsg{generation: m.gitGeneration, err: protocol.E("git_unavailable", "Git failed")})
	if m.git.State != "" || len(m.issues.active) != 1 {
		t.Fatal("Git failure hid uncertainty")
	}
	before := m.gitGeneration
	m, cmd = update(t, m, gitTickMsg{generation: before - 1})
	if cmd != nil || m.gitGeneration != before {
		t.Fatal("stale timer caused another query")
	}
}

func TestPickerMetadataStaysInOneRowWithoutChangingTargets(t *testing.T) {
	const name = "中文\nline\ttab\x1b]52;c;secret\a"
	for _, kind := range []string{"directory", "connections", "git"} {
		m, _ := selectionShell(t)
		m.width, m.height = 48, 16
		m.input.SetWidth(48)
		m.snapshot.Agent.Cwd = "/project/" + name
		m.snapshot.Agent.Model = "local/" + name
		m.selection = &selectionPanel{kind: kind, ready: true,
			directory:   protocol.DirectoryPage{Path: m.snapshot.Agent.Cwd, Entries: []protocol.DirectoryEntry{{Name: name}}},
			connections: protocol.ConnectionPage{Items: []protocol.ConnectionInfo{{ID: name, Label: name, Supported: true}}}}
		m.git = protocol.GitStatus{Path: m.snapshot.Agent.Cwd, State: "ready", Branch: name, Upstream: name}
		m.input.SetValue("kept")
		v := m.View()
		lines := strings.Split(ansi.Strip(v.Content), "\n")
		if len(lines) != 16 || !strings.Contains(lines[13], "kept") || strings.Contains(v.Content, "\x1b]52;") || strings.Contains(v.Content, "\t") {
			t.Fatal(kind, "metadata changed terminal geometry or controls", v.Content)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > 48 {
				t.Fatal(kind, "overflow")
			}
		}
		for _, hit := range m.layout().targets {
			if kind == "directory" && hit.call.name == "cd" && len(hit.call.args) > 0 && hit.call.args[0] != filepath.Join(m.snapshot.Agent.Cwd, name) {
				t.Fatal("display escaping rewrote path argument")
			}
		}
	}
}

func TestPickerCancellationReturnsToTheSameAnswerComposer(t *testing.T) {
	m := questionsShell(t, "one")
	_, client := selectionShell(t)
	m.client = client
	m.input.SetValue("main")
	m.dispatch(invocation{"answer", []string{"one"}})
	m.input.SetValue("answer")
	addDraftImage(t, &m.input, imageInfo("answer"))
	id := m.input.id
	m = selectionRead(t, m, "model")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.selection != nil || m.activeQuestion != "one" || m.input.id != id || m.input.Value() != "answer" || len(m.input.images) != 1 || m.mainDraft.Value() != "main" {
		t.Fatal("closing selection changed the answer's owner or draft")
	}
}

func TestPickerStillAllowsSkillCommandsBelowTheComposer(t *testing.T) {
	m, _ := selectionShell(t)
	m.input.SetValue("draft")
	m.skills, m.skillsReady = []protocol.ResourceInfo{uiSkill("review")}, true
	m = selectionRead(t, m, "cd")
	m = typeText(t, m, "$rev")
	if !m.paletteActive() || m.View().Cursor == nil || m.commandDraft == nil || m.commandDraft.Value() != "draft" {
		t.Fatal("modal picker blocked the reusable skill input")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.selection == nil || m.selection.kind != "directory" || m.input.Value() != "draft" || len(m.input.skills) != 1 || m.commandDraft != nil {
		t.Fatal("skill selection lost the picker or original draft")
	}
}

func TestPickerFocusDoesNotInterpretHeldDraftAsCommand(t *testing.T) {
	m, _ := selectionShell(t)
	m.input.SetValue("/literal kept")
	m = selectionRead(t, m, "cd")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd != nil || m.selection.index != 1 || m.input.Value() != "/literal kept" || m.View().Cursor != nil {
		t.Fatal("held command-like draft stole navigation focus")
	}
	m, cmd = update(t, m, tea.PasteMsg{Content: "\n/stop"})
	if m.selection != nil || m.pending != nil || m.input.Value() != "/literal kept\n/stop" || m.paletteActive() {
		t.Fatal("paste selected a row, executed a command or lost text")
	}
}

func TestPickerModelPagesUseTheOriginalVersionAndBoundQueries(t *testing.T) {
	m, client := selectionShell(t)
	handle := client.handle
	var queries []string
	client.handle = func(req protocol.Request, out any) error {
		err := handle(req, out)
		if err != nil {
			return err
		}
		switch req.Method {
		case "model.connections":
			var q protocol.ConnectionQuery
			if protocol.StrictDecode(req.Params, &q) != nil || q.Limit != 40 {
				t.Fatal(q)
			}
			queries = append(queries, q.After)
			if q.After != "" && (q.After != "two" || q.Version != "catalog-1") {
				t.Fatal("connection page lost catalog version", q)
			}
			out.(*protocol.ConnectionPage).Next = "two"
		case "model.list":
			var q protocol.ModelQuery
			if protocol.StrictDecode(req.Params, &q) != nil {
				t.Fatal(q)
			}
			queries = append(queries, q.After)
			if q.After != "" && q.After != "configured" {
				t.Fatal(q)
			}
			out.(*protocol.ModelPage).Next = "configured"
		}
		return nil
	}
	m = selectionRead(t, m, "model")
	m = selectionRead(t, m, "next")
	m = selectionRead(t, m, "model", "one")
	m = selectionRead(t, m, "next")
	if !reflect.DeepEqual(queries, []string{"", "two", "", "configured"}) || m.selection.base.Revision != 7 || m.pending != nil {
		t.Fatal("pagination fetched more than one requested page or changed revision", queries)
	}
}

func TestPickerCellsThemeAndClickCommandEquivalence(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.ASCII, colorprofile.ANSI, colorprofile.TrueColor} {
		for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
			for _, kind := range []string{"connections", "models", "efforts", "confirm", "directory", "git"} {
				t.Run(fmt.Sprint(profile, size, kind), func(t *testing.T) {
					makeShell := func() Shell {
						m, _ := selectionShell(t)
						m.width, m.height, m.theme.profile = size[0], size[1], profile
						m.input.SetWidth(m.width)
						m.input.setTheme(m.theme)
						m.input.SetValue("中文 draft")
						if kind == "directory" {
							m = selectionRead(t, m, "cd")
						} else if kind == "git" {
							m.selection = &selectionPanel{kind: "git"}
						} else {
							m = selectionRead(t, m, "model")
							if kind != "connections" {
								m = selectionRead(t, m, "model", "one")
							}
							if kind == "efforts" || kind == "confirm" {
								m = selectionLocal(t, m, "model", "one", "configured")
							}
							if kind == "confirm" {
								m = selectionLocal(t, m, "effort", "low")
							}
						}
						m.git = protocol.GitStatus{Path: "/project", State: "ready", Branch: "main", Staged: 1}
						return m
					}
					m := makeShell()
					f, v := m.layout(), m.View()
					lines := strings.Split(ansi.Strip(v.Content), "\n")
					if len(lines) != size[1] || !strings.Contains(lines[size[1]-3], "中文 draft") || strings.TrimRight(lines[size[1]-1], " ") != f.status {
						t.Fatal("picker displaced composer or status")
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > size[0] {
							t.Fatal("cell overflow", line)
						}
					}
					for _, target := range f.targets {
						if target.x < 0 || target.x+target.width > size[0] || target.y >= size[1] {
							t.Fatal("offscreen target", target)
						}
						if _, ok := lookupCommand(target.call.name); !ok {
							t.Fatal("click has no slash route", target)
						}
						mouse, _ := update(t, makeShell(), tea.MouseClickMsg{X: target.x, Y: target.y, Button: tea.MouseLeft})
						typed := makeShell()
						draft := typed.input
						typed.commandDraft = &draft
						typed.input = NewComposer()
						command := "/" + target.call.name
						for _, arg := range target.call.args {
							command += " '" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
						}
						typed.input.SetValue(command)
						typed, _ = update(t, typed, tea.KeyPressMsg{Code: tea.KeyEnter})
						if mouse.input.Value() != "中文 draft" || typed.input.Value() != "中文 draft" ||
							!reflect.DeepEqual(mouse.selection, typed.selection) || (mouse.pending == nil) != (typed.pending == nil) {
							t.Fatal("click and slash command diverged", target.call)
						}
						if mouse.pending != nil && string(mouse.pending.Params) != string(typed.pending.Params) {
							t.Fatal("mutation targets differ")
						}
					}
				})
			}
		}
	}
}
