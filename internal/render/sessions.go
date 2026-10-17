package render

import (
	"encoding/json"
	"path/filepath"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

const maxSessionDraftAgents = 16
const maxSessionDraftBytes = 32 << 20

type sessionDrafts struct {
	main    Composer
	answers map[string]*answerDraft
	active  string
}

type sessionOpening struct {
	agent      string
	generation uint64
}

type sessionOpenedMsg struct {
	from, agent string
	generation  uint64
	snapshot    protocol.Snapshot
	err         error
}

func composerHasDraft(c Composer) bool {
	return c.Value() != "" || len(c.images) > 0 || len(c.skills) > 0
}

// Only editors move between local cache slots. History and runtime state are
// read afresh from Core; no Agent, Task or process is stopped or duplicated.
func (m Shell) sessionDrafts() sessionDrafts {
	input := m.input
	if m.commandDraft != nil {
		input = *m.commandDraft
	}
	main := input
	if m.activeQuestion != "" && m.mainDraft != nil {
		main = *m.mainDraft
	}
	saved := sessionDrafts{answers: make(map[string]*answerDraft)}
	if composerHasDraft(main) {
		saved.main = main
	}
	for id, draft := range m.answerDrafts {
		copy := *draft
		if id == m.activeQuestion {
			copy.input = input
		}
		if composerHasDraft(copy.input) {
			saved.answers[id] = &copy
		}
	}
	if saved.answers[m.activeQuestion] != nil {
		saved.active = m.activeQuestion
	}
	return saved
}

func (d sessionDrafts) empty() bool { return d.main.id == "" && len(d.answers) == 0 }

func (d sessionDrafts) bytes() int {
	measure := func(c Composer, question *protocol.QuestionInfo) int {
		metadata, err := json.Marshal(struct {
			Images   []protocol.Attachment   `json:"images"`
			Skills   []protocol.ResourceInfo `json:"skills"`
			Question *protocol.QuestionInfo  `json:"question,omitempty"`
		}{c.images, c.skills, question})
		if err != nil {
			return maxSessionDraftBytes + 1
		}
		return len(c.Value()) + len(metadata)
	}
	n := 0
	if d.main.id != "" {
		n = measure(d.main, nil)
	}
	for _, answer := range d.answers {
		if n > maxSessionDraftBytes {
			return n
		}
		n += measure(answer.input, &answer.info)
	}
	return n
}

func (m Shell) sessionSwitchReady() error {
	if m.pending != nil {
		return protocol.E("request_pending", "resolve the original request with /retry before switching sessions")
	}
	if m.resizePending != nil {
		return protocol.E("request_pending", "resolve the original terminal size request with /resize before switching sessions")
	}
	if len(m.imageLoads) > 0 {
		return protocol.E("request_pending", "resolve the original image load in its draft with /image before switching sessions")
	}
	if m.copyFlow != nil && m.copyFlow.info.State != "completed" || m.attaching {
		return protocol.E("request_pending", "finish the current copy or terminal handoff before switching sessions")
	}
	return nil
}

func (m Shell) canSwitchSession(target string) error {
	if err := m.sessionSwitchReady(); err != nil {
		return err
	}
	saved := m.sessionDrafts()
	count, bytes := 0, 0
	for agent, drafts := range m.sessionEditors {
		if agent != target && agent != m.agent {
			count++
			bytes += drafts.bytes()
		}
	}
	if !saved.empty() {
		count++
		bytes += saved.bytes()
	}
	if count > maxSessionDraftAgents || bytes > maxSessionDraftBytes {
		return protocol.E("draft_limit", "inactive session drafts are limited to 16 sessions and 32 MiB; keep or clear drafts before switching")
	}
	return nil
}

func (m *Shell) sessionCommand(name string, args []string) (tea.Cmd, error) {
	if name == "sessions" || len(args) == 0 {
		after := ""
		if name == "sessions" && len(args) != 0 {
			after = args[0]
		}
		m.closePanel()
		m.selection = &selectionPanel{}
		return m.selectionQuery("sessions", "", after, false), nil
	}
	target := args[0]
	if target == "" || len(target) > 128 || !utf8.ValidString(target) {
		return nil, protocol.E("invalid_params", "session requires a retained Agent ID")
	}
	if target == m.agent {
		m.closePanel()
		return nil, nil
	}
	// Draft capacity is checked after typed commands have been consumed, when
	// the snapshot returns. A slash command is not an unsent conversation draft.
	if err := m.sessionSwitchReady(); err != nil {
		return nil, err
	}
	m.closePanel()
	m.sessionGeneration++
	generation, from := m.sessionGeneration, m.agent
	m.sessionOpening = &sessionOpening{agent: target, generation: generation}
	m.selection = &selectionPanel{kind: "session-opening", waiting: true}
	load := snapshotCmd(m.ctx, m.client, target)
	return func() tea.Msg {
		result := load().(snapshotMsg)
		return sessionOpenedMsg{from: from, agent: target, generation: generation, snapshot: result.Snapshot, err: result.Err}
	}, nil
}

func validSessionPage(page protocol.AgentPage, after string) bool {
	if len(page.Items) > 40 {
		return false
	}
	seen := make(map[string]bool)
	for _, item := range page.Items {
		if item.ID == "" || len(item.ID) > 128 || !utf8.ValidString(item.ID) || seen[item.ID] || !filepath.IsAbs(item.Cwd) || item.State == "" {
			return false
		}
		seen[item.ID] = true
	}
	return page.Next == "" || page.Next != after && len(page.Items) > 0 && page.Next == page.Items[len(page.Items)-1].ID
}

func (m *Shell) updateSessions(msg tea.Msg) (bool, tea.Cmd) {
	result, ok := msg.(sessionOpenedMsg)
	if !ok {
		return false, nil
	}
	opening := m.sessionOpening
	if opening == nil || result.from != m.agent || result.agent != opening.agent || result.generation != opening.generation || result.generation != m.sessionGeneration {
		return true, nil
	}
	m.sessionOpening = nil
	if result.err == nil && (result.snapshot.Agent.ID != result.agent || !filepath.IsAbs(result.snapshot.Agent.Cwd) || result.snapshot.Cursor < 0) {
		result.err = protocol.E("invalid_state", "session snapshot does not match the selected Agent")
	}
	if result.err == nil {
		result.err = validateQuestions(result.snapshot)
	}
	if result.err == nil {
		result.err = m.canSwitchSession(result.agent)
	}
	if result.err != nil {
		if m.selection != nil {
			m.selection.waiting = false
		}
		m.issues.raise("session", result.err)
		return true, nil
	}
	next := m.newSession(result.agent)
	if saved, exists := m.sessionEditors[result.agent]; exists {
		next.restoreSessionDrafts(saved)
	}
	if next.sessionEditors == nil {
		next.sessionEditors = make(map[string]sessionDrafts)
	}
	saved := m.sessionDrafts() // Includes edits made while the target was loading.
	delete(next.sessionEditors, result.agent)
	delete(next.sessionEditors, m.agent)
	if !saved.empty() {
		next.sessionEditors[m.agent] = saved
	}
	m.releaseSession()
	*m = next
	// The exact inspected snapshot starts the new projection; subsequent events
	// replay from its own cursor path rather than resubmitting any old work.
	updated, cmd := m.Update(snapshotMsg{Snapshot: result.snapshot, AgentID: result.agent})
	*m = updated.(Shell)
	return true, cmd
}

func (m Shell) newSession(agent string) Shell {
	root := m.rootCtx
	if root == nil {
		root = m.ctx
	}
	next := New(root, m.client, agent, m.emit)
	next.width, next.height, next.theme = m.width, m.height, m.theme
	next.input.SetWidth(next.width)
	next.input.setTheme(next.theme)
	next.foldCommentary, next.sessionEditors = m.foldCommentary, m.sessionEditors
	next.generation, next.selectionGeneration = m.generation+1, m.selectionGeneration+1
	next.processGeneration, next.imagesGeneration = m.processGeneration+1, m.imagesGeneration+1
	next.gitGeneration, next.activityGeneration = m.gitGeneration+1, m.activityGeneration+1
	next.skillsRequest, next.copyGeneration = m.skillsRequest+1, m.copyGeneration+1
	next.sessionGeneration = m.sessionGeneration + 1
	return next
}

func (m *Shell) restoreSessionDrafts(saved sessionDrafts) {
	main := saved.main
	if main.id == "" {
		main = NewComposer()
	}
	main.SetWidth(m.width)
	main.setTheme(m.theme)
	m.input, m.mainDraft, m.activeQuestion = main, nil, ""
	m.answerDrafts = make(map[string]*answerDraft)
	for id, answer := range saved.answers {
		copy := *answer
		if copy.info.State == "pending" {
			copy.info.State = "closed" // The new snapshot must prove it is still open.
		}
		copy.input.SetWidth(m.width)
		copy.input.setTheme(m.theme)
		m.answerDrafts[id] = &copy
	}
	if active := m.answerDrafts[saved.active]; active != nil {
		m.mainDraft, m.activeQuestion, m.input = &main, saved.active, active.input
	}
}

// Explicit copy restoration also respects drafts previously kept while this
// target was inactive. Empty cached slots never replace copied content.
func (m *Shell) mergeCachedSessionDrafts() (bool, error) {
	local, exists := m.sessionEditors[m.agent]
	if !exists {
		return false, nil
	}
	saved := m.sessionDrafts()
	if m.activeQuestion != "" {
		saved.active = m.activeQuestion
	}
	if local.main.id != "" {
		saved.main = local.main
	}
	for id, answer := range local.answers {
		saved.answers[id] = answer
	}
	if len(saved.answers) > maxAnswerDrafts {
		return false, protocol.E("draft_limit", "copied and cached answers exceed 16 drafts; clear source answers with /answer-clear or open the target with /session; retained copies remain queryable")
	}
	if local.active != "" {
		saved.active = local.active
	}
	m.restoreSessionDrafts(saved)
	delete(m.sessionEditors, m.agent)
	return true, nil
}

func (m *Shell) releaseSession() {
	if m.watchCancel != nil {
		m.watchCancel()
	}
	if m.sessionCancel != nil {
		m.sessionCancel()
	}
}
