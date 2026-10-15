package render

import (
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

func uiQuestion(id string) protocol.QuestionInfo {
	return protocol.QuestionInfo{ID: id, AgentID: "a", TaskID: "task", Prompt: "选择目录？\n中文 é 🙂 " + id, State: "pending"}
}

func questionsShell(t *testing.T, ids ...string) Shell {
	t.Helper()
	m := commandShell(t)
	for _, id := range ids {
		m.questions = append(m.questions, uiQuestion(id))
	}
	return m
}

func questionEvent(seq int64, kind string, q protocol.Question) protocol.Event {
	data, _ := json.Marshal(q)
	return protocol.Event{Version: protocol.Version, AgentID: q.AgentID, TaskID: q.TaskID, Seq: seq, Kind: kind, Payload: data}
}

func TestQuestionDraftSwitchingSharesCommandsNotEditors(t *testing.T) {
	m := questionsShell(t, "one", "two")
	const main = "主草稿\n中文 é 🙂 "
	m.input.SetValue(main)
	mainID := m.input.id
	addDraftImage(t, &m.input, imageInfo("main"))
	m = typeText(t, m, "/answer")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.paletteActive() || m.commandDraft == nil || len(m.palette.items) != 2 {
		t.Fatal("question candidates unavailable")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.activeQuestion != "one" || m.input.Value() != "" || m.mainDraft.Value() != main || len(m.input.images) != 0 {
		t.Fatal("question command consumed wrong editor")
	}
	m, _ = update(t, m, tea.PasteMsg{Content: "回答一\n多行 "})
	addDraftImage(t, &m.input, imageInfo("answer"))
	answerID := m.input.id
	m.dispatch(invocation{name: "answer", args: []string{"two"}})
	if m.input.id == answerID || m.input.Value() != "" || len(m.input.images) != 0 {
		t.Fatal("answer editors share state")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "回答二"})
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 48, Height: 16})
	m, _ = update(t, m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.dispatch(invocation{name: "back"})
	if m.activeQuestion != "" || m.input.id != mainID || m.input.Value() != main || !reflect.DeepEqual(m.input.imageIDs(), []string{"main"}) {
		t.Fatal("return lost main draft")
	}
	m.dispatch(invocation{name: "answer", args: []string{"one"}})
	if m.input.id != answerID || m.input.Value() != "回答一\n多行 " || !reflect.DeepEqual(m.input.imageIDs(), []string{"answer"}) || m.input.Width() != 46 {
		t.Fatal("return lost answer draft or resize", m.input.id == answerID, m.input.Value(), m.input.imageIDs(), m.input.Width())
	}
	m = typeText(t, m, "/back")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != main || m.activeQuestion != "" || m.answerDrafts["two"].input.Value() != "回答二" {
		t.Fatal("inline back overwrote destination")
	}
}

func TestQuestionImageReceiptsFollowHiddenDraftOwners(t *testing.T) {
	m := questionsShell(t, "one", "two")
	m.input.SetValue("main")
	mainID := m.input.id
	if _, err := m.imageCommand("image", []string{"main.png"}); err != nil {
		t.Fatal(err)
	}
	mainLoad := m.imageLoads[mainID]
	m.dispatch(invocation{name: "answer", args: []string{"one"}})
	answerID := m.input.id
	if _, err := m.imageCommand("image", []string{"answer.png"}); err != nil {
		t.Fatal(err)
	}
	answerLoad := m.imageLoads[answerID]
	m.dispatch(invocation{name: "answer", args: []string{"two"}})
	for _, load := range []imageLoad{mainLoad, answerLoad} {
		img := imageInfo(load.draft)
		m, _ = update(t, m, imageLoadedMsg{load: load, image: img, accepted: protocol.Accepted{OperationID: load.request.OperationID, AgentID: "a", State: "accepted", Attachment: &img}})
	}
	if len(m.input.images) != 0 || !reflect.DeepEqual(m.mainDraft.imageIDs(), []string{mainID}) || !reflect.DeepEqual(m.answerDrafts["one"].input.imageIDs(), []string{answerID}) {
		t.Fatal("image receipt attached to visible instead of owning editor", m.input.imageIDs(), m.mainDraft.imageIDs(), m.answerDrafts["one"].input.imageIDs(), m.err)
	}
}

func TestQuestionAnswerAcceptanceCannotEraseOtherOrEditedDraft(t *testing.T) {
	for _, mode := range []string{"same", "main", "other", "held", "edited", "aba", "image", "importing"} {
		t.Run(mode, func(t *testing.T) {
			m := questionsShell(t, "one", "two")
			m.input.SetValue("main")
			m.dispatch(invocation{name: "answer", args: []string{"one"}})
			m.input.SetValue("answer")
			addDraftImage(t, &m.input, imageInfo("one"))
			originalID := m.input.id
			m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if cmd == nil || m.pending == nil || m.pending.Method != "question.answer" {
				t.Fatal("answer became a new task")
			}
			req := *m.pending
			var p protocol.AnswerQuestion
			if protocol.StrictDecode(req.Params, &p) != nil || p.ID != "one" || p.Answer != "answer" || !reflect.DeepEqual(p.Attachments, []string{"one"}) {
				t.Fatal("answer input lost", p)
			}
			switch mode {
			case "main":
				m.dispatch(invocation{name: "back"})
			case "other":
				m.dispatch(invocation{name: "answer", args: []string{"two"}})
				m.input.SetValue("second")
			case "held":
				draft := m.input
				m.commandDraft = &draft
				m.input = NewComposer()
				m.input.SetValue("/images")
			case "edited":
				m.input.SetValue("changed")
			case "aba":
				m.input.SetValue("temporary")
				m.input.SetValue("answer")
			case "image":
				addDraftImage(t, &m.input, imageInfo("two"))
			case "importing":
				if _, err := m.imageCommand("image", []string{"new.png"}); err != nil {
					t.Fatal(err)
				}
			}
			accepted := protocol.Accepted{OperationID: req.OperationID, AgentID: "a", QuestionID: "one", TaskID: "task"}
			bad := accepted
			bad.QuestionID = "two"
			m, _ = update(t, m, acceptedMsg{Request: req, Accepted: bad})
			if m.pending == nil || m.findDraft(originalID) == nil {
				t.Fatal("mismatched receipt cleared request or draft")
			}
			m, _ = update(t, m, acceptedMsg{Request: req, Accepted: accepted})
			if m.pending != nil {
				t.Fatal("valid acceptance unresolved")
			}
			switch mode {
			case "same", "main":
				if m.activeQuestion != "" || m.input.Value() != "main" || m.answerDrafts["one"].input.Value() != "" {
					t.Fatal("main draft lost on answer acknowledgment")
				}
			case "other":
				if m.activeQuestion != "two" || m.input.Value() != "second" || m.answerDrafts["one"].input.Value() != "" {
					t.Fatal("late answer stole other question")
				}
			case "held":
				if m.input.Value() != "/images" || m.commandDraft.Value() != "" || len(m.commandDraft.images) != 0 {
					t.Fatal("late answer consumed temporary command")
				}
			default:
				if m.activeQuestion != "one" || len(m.input.images) == 0 || m.findDraft(originalID) == nil {
					t.Fatal("late acceptance erased newer answer")
				}
			}
		})
	}
}

func TestQuestionRejectionsRetryAndClosedFormsKeepInput(t *testing.T) {
	m := questionsShell(t, "one")
	m.dispatch(invocation{name: "answer"})
	addDraftImage(t, &m.input, imageInfo("one"))
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	req := *m.pending
	m, _ = update(t, m, acceptedMsg{Request: req, Err: protocol.E("transport_unknown", "lost")})
	if _, err := m.invoke(invocation{name: "retry"}); err != nil || !reflect.DeepEqual(req, *m.pending) {
		t.Fatal("answer retry changed request")
	}
	m.dispatch(invocation{name: "answer-clear"})
	if len(m.input.images) != 1 || m.err == nil {
		t.Fatal("clear discarded unresolved request")
	}
	m, _ = update(t, m, acceptedMsg{Request: req, Err: protocol.E("unsupported_capability", "no images")})
	if m.pending != nil || m.activeQuestion != "one" || len(m.input.images) != 1 {
		t.Fatal("definite rejection lost answer")
	}
	m.projectQuestions(protocol.Snapshot{Agent: protocol.Agent{ID: "a"}, Cursor: 4})
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || m.pending != nil || len(m.input.images) != 1 || m.answerDrafts["one"].info.State != "closed" {
		t.Fatal("closed answer submitted as task")
	}
	m.dispatch(invocation{name: "back"})
	if len(m.questionOptions()) != 1 {
		t.Fatal("closed dirty draft unreachable")
	}
	m.dispatch(invocation{name: "answer-clear", args: []string{"one"}})
	if len(m.questionOptions()) != 0 {
		t.Fatal("explicit discard did not release closed draft")
	}
}

func TestQuestionSnapshotsAndReplayDoNotReopenOrStealFocus(t *testing.T) {
	m := questionsShell(t, "one")
	m.input.SetValue("main")
	m.dispatch(invocation{name: "answer"})
	m.input.SetValue("unsent")
	m.watching = true
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "a"}, Cursor: 10}})
	q := protocol.Question{QuestionInfo: uiQuestion("one")}
	m, _ = update(t, m, coreEventMsg{questionEvent(1, "question.asked", q)})
	if len(m.questions) != 0 || m.answerDrafts["one"].info.State != "closed" || m.input.Value() != "unsent" {
		t.Fatal("old replay reopened form")
	}
	newQuestion := uiQuestion("two")
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "a"}, Cursor: 11, Questions: []protocol.QuestionInfo{newQuestion}}})
	if m.activeQuestion != "one" || m.input.Value() != "unsent" || len(m.questions) != 1 {
		t.Fatal("new question stole editor")
	}
	before := m.snapshot
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "a"}, Cursor: 12, Questions: []protocol.QuestionInfo{newQuestion, newQuestion}}})
	if !reflect.DeepEqual(before, m.snapshot) || m.err == nil {
		t.Fatal("invalid snapshot partially adopted")
	}
	m.projectQuestionEvent(questionEvent(13, "question.cancelled", protocol.Question{QuestionInfo: protocol.QuestionInfo{ID: "two", AgentID: "a", TaskID: "task", Prompt: newQuestion.Prompt, State: "cancelled"}}))
	m.projectQuestions(before)
	if len(m.questions) != 0 {
		t.Fatal("older snapshot reversed newer close event")
	}
}

func TestQuestionEventsRefreshCandidatesWithoutExecutingThem(t *testing.T) {
	m := questionsShell(t)
	m = typeText(t, m, "/answer ")
	if m.paletteActive() {
		t.Fatal("empty catalog advertised answers")
	}
	q := protocol.Question{QuestionInfo: uiQuestion("new")}
	m, cmd := update(t, m, coreEventMsg{questionEvent(1, "question.asked", q)})
	if cmd == nil || !m.refreshing || !m.paletteActive() || len(m.palette.items) != 1 || m.input.Value() != "/answer " || m.activeQuestion != "" || m.pending != nil {
		t.Fatal("question event did not refresh candidates or stole input")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m.input.SetValue("local answer")
	q.State, q.Answer = "answered", "answered by another client"
	m, _ = update(t, m, coreEventMsg{questionEvent(2, "question.answered", q)})
	if m.answerDrafts["new"].info.State != "answered" || m.input.Value() != "local answer" || len(m.questions) != 0 || !m.refreshAgain {
		t.Fatal("external answer erased draft or missed refresh")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil || m.pending != nil {
		t.Fatal("externally closed answer became another task")
	}
	m.dispatch(invocation{name: "back"})
	m = typeText(t, m, "/answer-clear ")
	if !m.paletteActive() || len(m.palette.items) != 1 {
		t.Fatal("retained closed draft not keyboard reachable")
	}
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.answerDrafts) != 0 || m.input.Value() != "" {
		t.Fatal("explicit local discard failed")
	}
}

func TestQuestionDraftLimitNeverEvictsUnsentAnswers(t *testing.T) {
	m := questionsShell(t)
	for i := range maxAnswerDrafts {
		id := fmt.Sprint(i)
		m.questions = []protocol.QuestionInfo{uiQuestion(id)}
		m.dispatch(invocation{name: "answer", args: []string{id}})
		m.input.SetValue("unsent " + id)
		m.dispatch(invocation{name: "back"})
		m.projectQuestions(protocol.Snapshot{Cursor: int64(i + 1)})
	}
	m.questions = []protocol.QuestionInfo{uiQuestion("new")}
	m.dispatch(invocation{name: "answer", args: []string{"new"}})
	if len(m.answerDrafts) != maxAnswerDrafts || m.activeQuestion != "" || m.err == nil || m.questionAction != nil {
		t.Fatal("draft limit evicted or switched editor")
	}
	m.dispatch(invocation{name: "answer-clear", args: []string{"0"}})
	m.dispatch(invocation{name: "answer", args: []string{"new"}})
	if m.activeQuestion != "new" || len(m.answerDrafts) != maxAnswerDrafts || m.answerDrafts["1"].input.Value() != "unsent 1" {
		t.Fatal("explicit draft release damaged other answers")
	}
}

func TestQuestionLayoutClickCommandParityAndNarrowWrapping(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 24}, {48, 16}} {
		for _, profile := range []colorprofile.Profile{colorprofile.ASCII, colorprofile.ANSI, colorprofile.TrueColor} {
			m := questionsShell(t, "one", "two")
			m, _ = update(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m, _ = update(t, m, tea.ColorProfileMsg{Profile: profile})
			m.questions[0].Prompt = strings.Repeat("中文 é 🙂\n", 100) + "\x1b[31m untrusted"
			m.dispatch(invocation{name: "questions"})
			for _, target := range m.layout().targets {
				if _, ok := lookupCommand(target.call.name); !ok {
					t.Fatal("click has no command")
				}
				if target.call.name == "answer" {
					clicked, typed := m, m
					clicked.click(target.x, target.y)
					typed.input.SetValue("/answer " + target.call.args[0])
					typed, _ = update(t, typed, tea.KeyPressMsg{Code: tea.KeyEnter})
					if clicked.activeQuestion != typed.activeQuestion || clicked.input.Value() != typed.input.Value() {
						t.Fatal("click and command disagree")
					}
				}
			}
			m.dispatch(invocation{name: "answer", args: []string{"one"}})
			m = typeText(t, m, "/answer ")
			lines := strings.Split(ansi.Strip(m.View().Content), "\n")
			if len(lines) != size[1] || len(m.layout().candidates) != 2 || !m.paletteActive() {
				t.Fatal("question palette/frame dimensions", size, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("question overflow")
				}
			}
		}
	}
}

func TestQuestionTranscriptValidatesBeforeProjectingAndBoundsAnswers(t *testing.T) {
	p := Transcript{Agent: protocol.Agent{ID: "a"}}
	q := protocol.Question{QuestionInfo: uiQuestion("one")}
	if err := p.Apply(questionEvent(1, "question.asked", q)); err != nil {
		t.Fatal(err)
	}
	q.State, q.Answer, q.Attachments = "answered", strings.Repeat("答", entryBytes), []string{"image"}
	if err := p.Apply(questionEvent(2, "question.answered", q)); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(questionEvent(2, "question.answered", q)); err != nil || len(p.Entries) != 2 || !p.Entries[1].Truncated || len(p.Entries[1].Text) > entryBytes || p.bytes > entryBytes+len(q.Prompt)+len("image") {
		t.Fatal("answer projection duplicate or unbounded", err)
	}
	if !strings.Contains(strings.Join(p.Lines(80, 24, 0, false), "\n"), "[img 1]") {
		t.Fatal("answer images missing from history")
	}
	q.Attachments = []string{"image", "image"}
	before := p.bytes
	if p.Apply(questionEvent(3, "question.answered", q)) == nil || p.Cursor != 2 || p.bytes != before {
		t.Fatal("malformed answer changed projection")
	}
}
