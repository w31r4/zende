package render

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

func uiSkill(id string) protocol.ResourceInfo {
	return protocol.ResourceInfo{ID: id, Version: strings.Repeat("a", 64), Kind: "skill", Name: "review", Description: "中文 review", Source: "/skills/" + id + "/SKILL.md", Files: 1, Bytes: 100}
}
func addDraftSkill(t *testing.T, c *Composer, info protocol.ResourceInfo) {
	t.Helper()
	if err := c.addSkill(info); err != nil {
		t.Fatal(err)
	}
}
func skillShell(t *testing.T) Shell {
	t.Helper()
	m := commandShell(t)
	m.skills, m.skillsReady = []protocol.ResourceInfo{uiSkill("one"), uiSkill("two")}, true
	return m
}

func TestSkillComposerVersionsOwnershipAndLimits(t *testing.T) {
	c := NewComposer()
	addDraftSkill(t, &c, uiSkill("one"))
	stamp := c.stamp()
	addDraftSkill(t, &c, uiSkill("one"))
	if c.stamp() != stamp {
		t.Fatal("duplicate selection edited draft")
	}
	next := uiSkill("one")
	next.Version = strings.Repeat("b", 64)
	if c.addSkill(next) == nil || c.stamp() != stamp {
		t.Fatal("refresh replaced selected body")
	}
	other := c
	addDraftSkill(t, &other, uiSkill("two"))
	if err := other.removeSkill("one"); err != nil {
		t.Fatal(err)
	}
	if len(c.skills) != 1 || c.skills[0].ID != "one" || len(other.skills) != 1 || other.skills[0].ID != "two" {
		t.Fatal("shared skill slice")
	}
	c.SetValue("text")
	c.ResetText()
	if len(c.skills) != 1 || c.id != stamp.id {
		t.Fatal("command reset lost refs")
	}
	for i := 1; i < protocol.MaxInputSkills; i++ {
		addDraftSkill(t, &c, uiSkill(fmt.Sprint(i)))
	}
	if c.addSkill(uiSkill("overflow")) == nil {
		t.Fatal("unbounded skills")
	}
	c.Reset()
	if c.id == stamp.id || len(c.skills) != 0 {
		t.Fatal("new draft kept old skills")
	}
}

func TestSkillCatalogMetadataOnlyGenerationPaginationAndBounds(t *testing.T) {
	client := &imageTestClient{}
	step := 0
	client.handle = func(req protocol.Request, target any) error {
		var q protocol.ResourceList
		if req.Method != "resource.list" || req.OperationID != "" || protocol.StrictDecode(req.Params, &q) != nil || q.AgentID != "a" || q.Kind != "skill" || q.Limit != 100 {
			t.Fatal(req, q)
		}
		step++
		page := protocol.ResourcePage{Generation: 2, Items: []protocol.ResourceInfo{uiSkill("two")}}
		switch step {
		case 1:
			if q.After != "" {
				t.Fatal(q)
			}
			page = protocol.ResourcePage{Generation: 1, Items: []protocol.ResourceInfo{uiSkill("one")}, Next: "one"}
		case 2:
			if q.After != "one" {
				t.Fatal(q)
			}
		case 3:
			if q.After != "" {
				t.Fatal("did not restart at beginning", q)
			}
		default:
			t.Fatal("unexpected request", step)
		}
		*target.(*protocol.ResourcePage) = page
		return nil
	}
	msg := skillCatalogCmd(context.Background(), client, "a", 9)().(skillCatalogMsg)
	if msg.err != nil || msg.request != 9 || msg.generation != 2 || len(msg.items) != 1 || msg.items[0].ID != "two" || step != 3 {
		t.Fatal(msg, step)
	}
	for _, kind := range []string{"order", "cursor", "version", "source", "negative", "size", "continuous"} {
		t.Run(kind, func(t *testing.T) {
			count := 0
			client.handle = func(_ protocol.Request, target any) error {
				count++
				page := protocol.ResourcePage{Generation: 1, Items: []protocol.ResourceInfo{uiSkill("one")}}
				switch kind {
				case "order":
					page.Items = append(page.Items, uiSkill("one"))
				case "cursor":
					page.Next = "missing"
				case "version":
					page.Items[0].Version = "latest"
				case "source":
					page.Items[0].Source = "relative"
				case "negative":
					page.Generation = -1
				case "size":
					page.Items = make([]protocol.ResourceInfo, 101)
				case "continuous":
					page.Generation, page.Next = int64(count), "one"
				}
				*target.(*protocol.ResourcePage) = page
				return nil
			}
			msg := skillCatalogCmd(context.Background(), client, "a", 1)().(skillCatalogMsg)
			if msg.err == nil || count > 6 {
				t.Fatal("invalid/unbounded catalog", msg, count)
			}
		})
	}
}

func TestSkillPickerPreservesTextImagesAndSelectionUsesNoIO(t *testing.T) {
	for _, prefix := range []string{"$", "/skill "} {
		t.Run(prefix, func(t *testing.T) {
			m := skillShell(t)
			addDraftImage(t, &m.input, imageInfo("image"))
			const draft = "中文 é 🙂\nreview "
			m, _ = update(t, m, tea.PasteMsg{Content: draft})
			if prefix == "$" {
				m = typeText(t, m, prefix+"review")
			} else {
				m.dispatch(invocation{name: "skill"})
				// The command starts a read but selection itself is local.
			}
			if !m.paletteActive() || len(m.palette.items) != 2 {
				t.Fatal("missing choices", m.input.Value())
			}
			m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
			m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd != nil || m.pending != nil || m.commandDraft != nil || m.input.Value() != draft || len(m.input.images) != 1 || len(m.input.skills) != 1 || m.input.skills[0].ID != "two" {
				t.Fatal("selection lost or submitted draft", m.input.Value(), m.input.skills)
			}
			m = typeText(t, m, "/skill-remove")
			m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
			m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.input.Value() != draft || len(m.input.skills) != 0 || len(m.input.images) != 1 {
				t.Fatal("remove lost draft")
			}
		})
	}
}

func TestSkillPickerLoadingEmptyEscapePasteAndLateReplies(t *testing.T) {
	m := commandShell(t)
	client := &imageTestClient{handle: func(_ protocol.Request, out any) error {
		*out.(*protocol.ResourcePage) = protocol.ResourcePage{Generation: 4, Items: []protocol.ResourceInfo{uiSkill("one")}}
		return nil
	}}
	m.client = client
	m = typeText(t, m, "/skill")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if cmd == nil || !m.skillsWaiting || len(client.requests) != 0 || len(m.layout().candidates) != 1 {
		t.Fatal("Tab did not start deferred metadata read")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending != nil {
		t.Fatal("loading submitted command")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m, _ = update(t, m, cmd())
	if m.paletteActive() || len(m.skills) != 1 || m.skillsWaiting {
		t.Fatal("late catalog reopened picker")
	}
	m.input.ResetText()
	m, _ = update(t, m, tea.PasteMsg{Content: "$review"})
	if m.paletteActive() {
		t.Fatal("paste opened picker")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending == nil {
		t.Fatal("literal pasted text not submitted")
	}
	var p protocol.Submit
	if protocol.StrictDecode(m.pending.Params, &p) != nil || p.Input != "$review" || len(p.Skills) != 0 {
		t.Fatal(p)
	}
	m = skillShell(t)
	m = typeText(t, m, "$nothing")
	if m.paletteActive() || len(m.layout().candidates) != 1 {
		t.Fatal("missing empty search state")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending != nil {
		t.Fatal("empty picker submitted literal")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending == nil {
		t.Fatal("escaped literal cannot submit")
	}
}

func TestSkillRefreshKeepsSelectedVersionAndIgnoresOldAttempt(t *testing.T) {
	m := skillShell(t)
	addDraftSkill(t, &m.input, uiSkill("one"))
	m = typeText(t, m, "$")
	m.skillsFloor = 10
	cmd := m.loadSkills(false)
	if cmd == nil || m.loadSkills(true) != nil {
		t.Fatal("loads not coalesced")
	}
	request := m.skillsRequest
	m, _ = update(t, m, skillCatalogMsg{request: request - 1, generation: 12})
	if !m.skillsWaiting {
		t.Fatal("old attempt consumed pending read")
	}
	m, cmd = update(t, m, skillCatalogMsg{request: request, generation: 9, items: m.skills})
	if cmd == nil || !m.skillsWaiting || m.skillsRequest == request {
		t.Fatal("publication race not refreshed")
	}
	next := uiSkill("one")
	next.Version = strings.Repeat("b", 64)
	m, _ = update(t, m, skillCatalogMsg{request: m.skillsRequest, generation: 10, items: []protocol.ResourceInfo{next}})
	if !m.paletteActive() || m.palette.items[0].args[1] != next.Version || m.input.skills[0].Version == next.Version {
		t.Fatal("catalog refresh rewrote selected draft")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.err == nil || m.input.skills[0].Version == next.Version {
		t.Fatal("selection silently replaced version")
	}
}

func TestSkillCatalogBehindEventCursorStopsAutomaticRetry(t *testing.T) {
	m := skillShell(t)
	m.skillsFloor = 10
	m.loadSkills(false)
	for i := range 3 {
		var cmd tea.Cmd
		m, cmd = update(t, m, skillCatalogMsg{request: m.skillsRequest, generation: 9})
		if i < 2 && cmd == nil || i == 2 && (cmd != nil || m.skillsWaiting) {
			t.Fatal("unbounded stale catalog retry", i)
		}
	}
	cmd, err := m.skillCommand("skill", nil)
	if err != nil || cmd == nil || m.skillsStale != 0 {
		t.Fatal("explicit retry unavailable", err)
	}
}

func TestSkillTranscriptReferencesAreBoundedAndValidated(t *testing.T) {
	ref := protocol.ResourceRef{ID: "skill", Version: strings.Repeat("a", 64)}
	p := Transcript{Agent: protocol.Agent{ID: "agent"}}
	task := protocol.Task{ID: "task", AgentID: "agent", State: "accepted", Skills: []protocol.ResourceRef{ref}}
	appendProjection(t, &p, "task.accepted", task)
	size := p.bytes
	task.State = "running"
	appendProjection(t, &p, "task.started", task)
	if p.bytes != size || len(p.Entries) != 1 || !reflect.DeepEqual(p.Entries[0].Skills, task.Skills) {
		t.Fatal("double-counted input refs")
	}
	q := protocol.Question{QuestionInfo: protocol.QuestionInfo{ID: "q", AgentID: "agent", TaskID: "task", Prompt: "Choose", State: "answered"}, Skills: []protocol.ResourceRef{ref}}
	appendProjection(t, &p, "question.answered", q)
	if len(p.Entries) != 3 || !reflect.DeepEqual(p.Entries[2].Skills, q.Skills) {
		t.Fatal("skill-only answer missing")
	}
	cursor, size := p.Cursor, p.bytes
	task.Skills[0].Version = "latest"
	raw, _ := json.Marshal(task)
	if p.Apply(protocol.Event{Version: 1, AgentID: "agent", TaskID: "task", Seq: cursor + 1, Kind: "task.started", Payload: raw}) == nil || p.Cursor != cursor || p.bytes != size {
		t.Fatal("invalid event advanced projection")
	}
	for i := range 520 {
		task.ID, task.Skills[0].Version = fmt.Sprint(i), ref.Version
		raw, _ := json.Marshal(task)
		if err := p.Apply(protocol.Event{Version: 1, AgentID: "agent", TaskID: task.ID, Seq: p.Cursor + 1, Kind: "task.accepted", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for _, entry := range p.Entries {
		total += len(entry.Text)
		for _, r := range entry.Skills {
			total += len(r.ID) + len(r.Version)
		}
		for _, id := range entry.Attachments {
			total += len(id)
		}
	}
	if p.DroppedThrough == 0 || p.bytes != total {
		t.Fatal("eviction lost byte accounting", p.bytes, total)
	}
}

func TestSkillOnlyMainAndAnswerLateAcceptance(t *testing.T) {
	for _, answer := range []bool{false, true} {
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprint(answer, edit), func(t *testing.T) {
				m := questionsShell(t, "q")
				if answer {
					m.dispatch(invocation{name: "answer", args: []string{"q"}})
				}
				addDraftSkill(t, &m.input, uiSkill("one"))
				m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
				if cmd == nil || m.pending == nil {
					t.Fatal("skill-only input ignored")
				}
				req := *m.pending
				want := m.input.skillRefs()
				if answer {
					var p protocol.AnswerQuestion
					if protocol.StrictDecode(req.Params, &p) != nil || p.ID != "q" || !reflect.DeepEqual(p.Skills, want) {
						t.Fatal(p)
					}
				} else {
					var p protocol.Submit
					if protocol.StrictDecode(req.Params, &p) != nil || !reflect.DeepEqual(p.Skills, want) {
						t.Fatal(p)
					}
				}
				if edit {
					addDraftSkill(t, &m.input, uiSkill("two"))
				}
				if answer {
					m.dispatch(invocation{name: "back"})
				}
				m, _ = update(t, m, acceptedMsg{Request: req, Accepted: protocol.Accepted{OperationID: req.OperationID, AgentID: m.agent, QuestionID: "q", TaskID: "task"}})
				var refs []protocol.ResourceRef
				if answer {
					if d := m.answerDrafts["q"]; d != nil {
						refs = d.input.skillRefs()
					}
					if len(m.input.skills) != 0 {
						t.Fatal("answer acceptance changed main draft")
					}
				} else {
					refs = m.input.skillRefs()
				}
				if edit && len(refs) != 2 || !edit && len(refs) != 0 {
					t.Fatal("late acknowledgement erased newer refs", refs)
				}
			})
		}
	}
}

func TestSkillLayoutRemainsBoundedAndEveryTargetUsesRegistry(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
		for _, profile := range []colorprofile.Profile{colorprofile.NoTTY, colorprofile.ANSI, colorprofile.TrueColor} {
			m := skillShell(t)
			m, _ = update(t, m, tea.ColorProfileMsg{Profile: profile})
			m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			addDraftImage(t, &m.input, imageInfo("image"))
			addDraftSkill(t, &m.input, uiSkill("one"))
			m = typeText(t, m, "$")
			f, v := m.layout(), m.View()
			lines := strings.Split(ansi.Strip(v.Content), "\n")
			if len(lines) != size[1] || v.Cursor == nil || v.Cursor.Y != len(f.body)+2 || len(f.candidates) != 2 {
				t.Fatal("bad skill frame", size, v.Cursor, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("overflow", line)
				}
			}
			found := false
			for _, target := range f.targets {
				if _, ok := lookupCommand(target.call.name); !ok {
					t.Fatal("mouse-only action", target)
				}
				if target.call.name == "skill-remove" {
					found = true
				}
			}
			if !found {
				t.Fatal("skill strip missing removal route")
			}
		}
	}
}
