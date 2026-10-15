package render

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

const maxAnswerDrafts = 16

type answerDraft struct {
	info  protocol.QuestionInfo
	input Composer // The active editor lives in Shell.input, not in this slot.
}

type questionAction struct{ name, id string }

func validQuestion(q protocol.QuestionInfo, agent string) bool {
	return q.AgentID == agent && q.ID != "" && len(q.ID) <= 128 && utf8.ValidString(q.ID) &&
		q.TaskID != "" && len(q.TaskID) <= 128 && utf8.ValidString(q.TaskID) &&
		strings.TrimSpace(q.Prompt) != "" && len(q.Prompt) <= protocol.MaxQuestionBytes && utf8.ValidString(q.Prompt) &&
		(q.State == "pending" || q.State == "answered" || q.State == "cancelled") &&
		q.DeliveredSeq >= 0 && (q.State == "answered" || q.DeliveredSeq == 0)
}

func validateQuestions(snapshot protocol.Snapshot) error {
	seen := make(map[string]bool)
	if len(snapshot.Questions) > protocol.MaxOpenQuestions {
		return protocol.E("invalid_state", "too many pending questions")
	}
	for _, q := range snapshot.Questions {
		if !validQuestion(q, snapshot.Agent.ID) || q.State != "pending" || seen[q.ID] {
			return protocol.E("invalid_state", "invalid pending question")
		}
		seen[q.ID] = true
	}
	return nil
}

func (m *Shell) projectQuestions(snapshot protocol.Snapshot) {
	if snapshot.Cursor < m.questionCursor {
		return
	}
	m.questionCursor = snapshot.Cursor
	m.questions = append([]protocol.QuestionInfo(nil), snapshot.Questions...)
	for id, draft := range m.answerDrafts {
		if draft.info.State == "pending" {
			draft.info.State = "closed" // A snapshot need not include terminal answers.
		}
		for _, q := range m.questions {
			if q.ID == id {
				draft.info = q
			}
		}
	}
	m.refreshQuestionPalette()
}

// The transcript has already validated this event. A newer snapshot wins over
// replay, so replaying an old question cannot reopen an already closed form.
func (m *Shell) projectQuestionEvent(e protocol.Event) {
	if e.Seq <= m.questionCursor || (e.Kind != "question.asked" && e.Kind != "question.answered" && e.Kind != "question.cancelled") {
		return
	}
	var q protocol.QuestionInfo
	if decodePayload(e, &q) != nil {
		return
	}
	m.questionCursor = e.Seq
	next := make([]protocol.QuestionInfo, 0, len(m.questions)+1)
	for _, old := range m.questions {
		if old.ID != q.ID {
			next = append(next, old)
		}
	}
	if q.State == "pending" && len(next) < protocol.MaxOpenQuestions {
		next = append(next, q)
	}
	m.questions = next
	if draft := m.answerDrafts[q.ID]; draft != nil {
		draft.info = q
	}
	m.refreshQuestionPalette()
}

func (m Shell) questionOptions() []protocol.QuestionInfo {
	options := append([]protocol.QuestionInfo(nil), m.questions...)
	seen := make(map[string]bool)
	for _, q := range options {
		seen[q.ID] = true
	}
	for id, draft := range m.answerDrafts {
		if !seen[id] && (id == m.activeQuestion || m.keepAnswerDraft(id, draft)) {
			options = append(options, draft.info)
		}
	}
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return options
}

func (m Shell) keepAnswerDraft(id string, draft *answerDraft) bool {
	c := &draft.input
	if id == m.activeQuestion {
		c = &m.input
		if m.commandDraft != nil {
			c = m.commandDraft
		}
	}
	_, loading := m.imageLoads[c.id]
	return c.Value() != "" || len(c.images) > 0 || len(c.skills) > 0 || loading || m.sentDraft.id == c.id && m.pending != nil
}

func (m *Shell) questionCommand(name string, args []string) error {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	if name == "answer" {
		options := m.questionOptions()
		if id == "" {
			if len(options) == 1 {
				id = options[0].ID
			} else {
				name = "questions"
			}
		}
		if id != "" {
			found := false
			for _, q := range options {
				found = found || q.ID == id
			}
			if !found {
				return protocol.E("not_found", "question is not pending and has no local draft")
			}
			for key, draft := range m.answerDrafts {
				if key != m.activeQuestion && draft.info.State != "pending" && !m.keepAnswerDraft(key, draft) {
					delete(m.answerDrafts, key)
				}
			}
			if m.answerDrafts[id] == nil && len(m.answerDrafts) >= maxAnswerDrafts {
				return protocol.E("draft_limit", "16 answer drafts retained; /answer-clear QUESTION discards a local draft")
			}
		}
	}
	if name == "answer-clear" {
		if id == "" {
			id = m.activeQuestion
		}
		draft := m.answerDrafts[id]
		if draft == nil {
			return protocol.E("not_found", "no local answer draft")
		}
		c := &draft.input
		if id == m.activeQuestion {
			c = m.editDraft()
		}
		if _, loading := m.imageLoads[c.id]; loading || m.pending != nil && m.sentDraft.id == c.id {
			return protocol.E("request_pending", "resolve the original image or answer request before clearing its draft")
		}
	}
	// Typed commands are consumed before the transition; mouse dispatch preserves
	// its current editor. Neither path can accidentally clear the new editor.
	m.questionAction = &questionAction{name, id}
	return nil
}

func (m *Shell) applyQuestionAction() {
	action := m.questionAction
	if action == nil {
		return
	}
	m.questionAction = nil
	if m.commandDraft != nil {
		m.restoreCommandDraft()
	}
	if action.name == "answer-clear" {
		if action.id == m.activeQuestion {
			m.input.Reset()
		} else {
			delete(m.answerDrafts, action.id)
		}
		m.refreshQuestionPalette()
		return
	}
	m.closePanel()
	target := ""
	if action.name == "answer" {
		target = action.id
	}
	m.switchQuestion(target)
	m.showQuestions = action.name == "questions"
}

func (m *Shell) switchQuestion(id string) {
	if id == m.activeQuestion {
		return
	}
	if m.activeQuestion != "" {
		m.answerDrafts[m.activeQuestion].input = m.input
	} else {
		draft := m.input
		m.mainDraft = &draft
	}
	if id == "" {
		m.input = *m.mainDraft
		m.mainDraft = nil
	} else {
		if m.answerDrafts == nil {
			m.answerDrafts = make(map[string]*answerDraft)
		}
		if m.answerDrafts[id] == nil {
			for _, q := range m.questions {
				if q.ID == id {
					m.answerDrafts[id] = &answerDraft{info: q, input: NewComposer()}
					break
				}
			}
		}
		m.input = m.answerDrafts[id].input
	}
	m.activeQuestion = id
	m.input.SetWidth(m.width)
	m.input.setTheme(m.theme)
	m.palette = commandPalette{}
}

func (m *Shell) refreshQuestionPalette() {
	if m.palette.entry == "answer" || m.palette.entry == "answer-clear" {
		index := m.palette.index
		m.palette.items = nil
		m.syncPaletteKey()
		m.palette.index = min(index, max(0, len(m.palette.items)-1))
	}
}

func questionTitle(q protocol.QuestionInfo) string {
	prefix := "? "
	if q.State != "pending" {
		prefix += "[" + q.State + "] "
	}
	return prefix + strings.Join(strings.Fields(safeText(q.Prompt)), " ")
}

func (m Shell) questionRows(f *frame, rows int) {
	if !m.showQuestions && m.activeQuestion != "" {
		q := m.answerDrafts[m.activeQuestion].info
		prefix := "? "
		if q.State != "pending" {
			prefix += "[" + q.State + "] "
		}
		lines := strings.Split(ansi.Hardwrap(prefix+safeText(q.Prompt), m.width, true), "\n")
		start := min(m.scroll, max(0, len(lines)-rows))
		f.body = append(f.body, lines[start:min(len(lines), start+rows)]...)
		return
	}
	options := m.questionOptions()
	if len(options) == 0 {
		f.body = append(f.body, "No pending questions")
	}
	start := min(m.scroll, max(0, len(options)-rows))
	for _, q := range options[start:min(len(options), start+rows)] {
		text := ansi.Truncate(questionTitle(q), m.width, "...")
		f.targets = append(f.targets, hitTarget{x: 0, y: len(f.body), width: ansi.StringWidth(text), call: invocation{name: "answer", args: []string{q.ID}}, candidate: -1})
		f.body = append(f.body, text)
	}
}
