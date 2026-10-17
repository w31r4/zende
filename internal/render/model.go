// Package render projects core events into a terminal UI.
package render

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

// Shell owns local input/layout and last observed core data, not task lifetime.
type Shell struct {
	ctx                                          context.Context
	client                                       Client
	emit                                         func(tea.Msg)
	agent                                        string
	snapshot                                     protocol.Snapshot
	transcript                                   Transcript
	cursor                                       int64
	input                                        Composer
	theme                                        theme
	issues                                       issues
	activity                                     protocol.Activity
	activityWaiting, activityStale, showActivity bool
	activityGeneration                           uint64
	width, height                                int
	loading, watching, refreshing, refreshAgain  bool
	pending                                      *protocol.Request
	sentText                                     string
	sentDraft                                    draftStamp
	imageLoads                                   map[string]imageLoad
	imagesPanel                                  *imagesPanel
	imagesGeneration                             uint64
	skills                                       []protocol.ResourceInfo
	skillsReady, skillsWaiting                   bool
	skillsRequest                                uint64
	skillsGeneration, skillsFloor                int64
	skillsStale                                  int
	skillPicker                                  string
	questions                                    []protocol.QuestionInfo
	questionCursor                               int64
	showQuestions                                bool
	activeQuestion                               string
	answerDrafts                                 map[string]*answerDraft
	mainDraft                                    *Composer
	questionAction                               *questionAction
	sentQuestion                                 protocol.QuestionInfo
	err                                          error
	help                                         bool
	foldCommentary                               bool
	commentaryOverrides                          map[string]bool
	palette                                      commandPalette
	commandDraft                                 *Composer
	scroll                                       int
	generation                                   uint64
	watchCancel                                  context.CancelFunc
	connected, retryPending, gap, haveSnapshot   bool
	retryAttempt                                 int
	connectionErr                                error
	panel                                        *processPanel
	processGeneration                            uint64
	resizePending                                *protocol.Request
	attaching                                    bool
	selection                                    *selectionPanel
	selectionGeneration, selectionMutation       uint64
	git                                          protocol.GitStatus
	gitPath                                      string
	gitGeneration                                uint64
	gitWaiting                                   bool
}

func New(ctx context.Context, client Client, agent string, emit func(tea.Msg)) Shell {
	input := NewComposer()
	return Shell{ctx: ctx, client: client, agent: agent, emit: emit, input: input, width: 80, height: 24, loading: true,
		transcript: Transcript{Agent: protocol.Agent{ID: agent}}}
}

func (m Shell) Init() tea.Cmd { return snapshotCmd(m.ctx, m.client, m.agent) }

func (m Shell) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.updateSelection(msg); handled {
		return m, cmd
	}
	if handled, cmd := m.updateImages(msg); handled {
		m.refreshImagePalette()
		return m, cmd
	}
	if handled, cmd := m.updateProcess(msg); handled {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if m.selection != nil && m.commandDraft == nil {
			m.closePanel() // Pasting returns to the held composer, never chooses a row.
		}
	case skillCatalogMsg:
		if msg.request != m.skillsRequest || !m.skillsWaiting {
			return m, nil
		}
		m.skillsWaiting = false
		m.issues.raise("skills", msg.err)
		if msg.err == nil {
			if msg.generation < m.skillsFloor {
				m.skillsStale++
				if m.skillsStale >= 3 {
					m.issues.raise("skills", protocol.E("resource_changed", "skill catalog is behind events; open /skill to retry"))
					return m, nil
				}
				cmd := m.loadSkills(true)
				return m, cmd
			}
			m.skillsStale = 0
			m.skills, m.skillsGeneration, m.skillsReady = msg.items, msg.generation, true
			m.refreshSkillPalette()
		}
		return m, nil
	case tea.ColorProfileMsg:
		m.theme.profile = msg.Profile
		m.input.setTheme(m.theme)
		if m.commandDraft != nil {
			m.commandDraft.setTheme(m.theme)
		}
		return m, nil
	case activityMsg:
		if msg.Generation != m.activityGeneration {
			return m, nil
		}
		m.activityWaiting = false
		m.activityStale = msg.Err != nil
		if msg.Err != nil {
			m.issues.raise("activity", msg.Err)
		} else {
			m.activity = msg.Activity
			if len(msg.Activity.Unavailable) > 0 {
				m.issues.raise("activity", protocol.E("source_unavailable", "background process state is unavailable"))
			} else {
				m.issues.resolve("activity")
			}
		}
		return m, activityTick(m.ctx, m.activityGeneration)
	case activityTickMsg:
		if msg.Generation != m.activityGeneration {
			return m, nil
		}
		cmd := m.startActivity()
		return m, cmd
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.input.SetWidth(m.width)
		if m.commandDraft != nil {
			m.commandDraft.SetWidth(m.width)
		}
		cmd := m.resizeSelected()
		return m, cmd
	case snapshotMsg:
		m.loading, m.refreshing = false, false
		if msg.Err != nil {
			m.reportConnection(msg.Err)
			cmd := m.reconnect()
			return m, cmd
		}
		if msg.Snapshot.Agent.ID != m.agent {
			m.report(protocol.E("invalid_state", "snapshot belongs to another Agent"))
			return m, nil
		}
		if err := validateQuestions(msg.Snapshot); err != nil {
			m.report(err)
			return m, nil
		}
		if msg.Snapshot.Cursor >= m.snapshot.Cursor {
			m.snapshot = msg.Snapshot
			m.projectQuestions(msg.Snapshot)
		}
		m.haveSnapshot = true
		git := m.syncGit(false)
		if !m.watching && !m.gap && !m.attaching {
			// Replay from our actual projection cursor, never the newer snapshot.
			// The stream provides backpressure while old history is consumed.
			cmd := m.startWatch()
			activity := m.startActivity()
			return m, tea.Batch(cmd, activity, git)
		}
		if m.refreshAgain {
			m.refreshAgain, m.refreshing = false, true
			return m, tea.Batch(snapshotCmd(m.ctx, m.client, m.agent), git)
		}
		return m, git
	case watchReadyMsg:
		if msg.Generation == m.generation && m.watching {
			m.connected, m.connectionErr = true, nil
			m.issues.resolve("connection")
		}
		return m, nil
	case watchEventMsg:
		if msg.Generation != m.generation || !m.watching {
			return m, nil
		}
		m.connected, m.connectionErr, m.retryAttempt = true, nil, 0
		m.issues.resolve("connection")
		return m.Update(coreEventMsg{msg.Event})
	case coreEventMsg:
		if err := m.transcript.Apply(msg.Event); err != nil {
			m.connectionErr, m.gap, m.watching, m.connected = err, true, false, false
			m.reportConnection(err)
			if m.watchCancel != nil {
				m.watchCancel()
			}
			m.generation++ // Discard messages already queued by the failed stream.
			return m, nil
		}
		if msg.Event.Seq <= m.cursor {
			return m, nil
		}
		m.cursor = m.transcript.Cursor
		if msg.Event.CopiedFrom != nil {
			return m, nil
		}
		m.projectQuestionEvent(msg.Event)
		if msg.Event.Kind == "context.compaction.state" {
			var info protocol.CompactionInfo
			// Transcript.Apply already checked the event before advancing.
			if decodePayload(msg.Event, &info) == nil {
				if info.State == "failed" {
					m.issues.raise("compaction", info.Failure)
				} else if info.State == "completed" || info.State == "cancelled" {
					m.issues.resolve("compaction")
				}
			}
		}
		if msg.Event.Kind == "resource.published" {
			m.skillsFloor = max(m.skillsFloor, msg.Event.Seq)
			if m.palette.entry == "skill" {
				cmd := m.loadSkills(false)
				return m, cmd
			}
		}
		// State refreshes are coalesced, not one request per model delta.
		if m.cursor >= m.snapshot.Cursor && (strings.HasPrefix(msg.Event.Kind, "task.") || strings.HasPrefix(msg.Event.Kind, "agent.") || strings.HasPrefix(msg.Event.Kind, "question.")) {
			if m.refreshing {
				m.refreshAgain = true
				return m, nil
			}
			m.refreshing = true
			return m, snapshotCmd(m.ctx, m.client, m.agent)
		}
		return m, nil
	case watchEndedMsg:
		if msg.Generation != m.generation {
			return m, nil
		}
		m.watching, m.connected = false, false
		if m.watchCancel != nil {
			m.watchCancel()
		}
		if m.ctx.Err() == nil {
			m.connectionErr = protocol.E("disconnected", "reconnecting")
			var known *protocol.Error
			if errors.As(msg.Err, &known) {
				m.connectionErr = known
				switch known.Data.Kind {
				case "cursor_gap", "invalid_state", "invalid_request", "not_found", "output_limit":
					m.gap = true
				}
			}
			m.reportConnection(m.connectionErr)
		}
		cmd := m.reconnect()
		return m, cmd
	case reconnectMsg:
		if msg.Generation != m.generation || !m.retryPending {
			return m, nil
		}
		m.retryPending = false
		if m.watching || m.refreshing || m.gap || m.ctx.Err() != nil {
			return m, nil
		}
		if !m.haveSnapshot {
			m.refreshing = true
			return m, snapshotCmd(m.ctx, m.client, m.agent)
		}
		cmd := m.startWatch()
		return m, cmd
	case acceptedMsg:
		if m.pending == nil || m.pending.OperationID != msg.Request.OperationID {
			return m, nil
		}
		if msg.Err != nil {
			m.report(msg.Err)
			var known *protocol.Error
			if errors.As(msg.Err, &known) && known.Data.Kind != "transport_unknown" && known.Data.Kind != "unavailable" {
				m.pending = nil // A definite rejection permits correcting the input.
			}
			return m, nil
		}
		if msg.Accepted.OperationID != m.pending.OperationID || msg.Accepted.AgentID != m.agent {
			m.report(protocol.E("transport_unknown", "acceptance identity did not match; query the original request"))
			return m, nil
		}
		if m.pending.Method == "question.answer" && (msg.Accepted.QuestionID != m.sentQuestion.ID || msg.Accepted.TaskID != m.sentQuestion.TaskID) {
			m.report(protocol.E("transport_unknown", "answer acceptance identity did not match; retry the original request"))
			return m, nil
		}
		if m.pending.Method == "agent.select" {
			if msg.Accepted.Selection == nil || msg.Accepted.Selection.AgentID != m.agent {
				m.report(protocol.E("transport_unknown", "selection acceptance is incomplete; /retry keeps its ID"))
				return m, nil
			}
			if m.selection != nil && m.selectionGeneration == m.selectionMutation {
				m.closePanel()
			}
			// Invalidate even a query already in flight for the previous cwd.
			m.git, m.gitPath, m.gitWaiting = protocol.GitStatus{}, "", false
			m.gitGeneration++
		}
		m.pending = nil
		if draft := m.findDraft(m.sentDraft.id); draft != nil && draft.stamp() == m.sentDraft && draft.Value() == m.sentText {
			draft.Reset()
			if m.sentQuestion.ID != "" && m.activeQuestion == m.sentQuestion.ID && m.commandDraft == nil {
				m.closePanel()
				m.switchQuestion("")
			}
		}
		m.sentText, m.err = "", nil
		m.sentDraft = draftStamp{}
		m.sentQuestion = protocol.QuestionInfo{}
		m.issues.resolve("request")
		if m.refreshing {
			m.refreshAgain = true
			return m, nil
		}
		m.refreshing = true
		return m, snapshotCmd(m.ctx, m.client, m.agent)
	case tea.ResumeMsg:
		if !m.refreshing {
			m.refreshing = true
			return m, snapshotCmd(m.ctx, m.client, m.agent)
		}
		return m, nil
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Mod == 0 {
			cmd := m.click(msg.X, msg.Y)
			return m, cmd
		}
		return m, nil
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollBy(3)
		case tea.MouseWheelDown:
			m.scrollBy(-3)
		}
		return m, nil
	case tea.KeyPressMsg:
		if handled, cmd := m.paletteKey(msg.String()); handled {
			return m, cmd
		}
		if msg.String() == "esc" && m.commandDraft != nil {
			m.restoreCommandDraft()
			return m, nil
		}
		if handled, cmd := m.panelKey(msg.String()); handled {
			return m, cmd
		}
		if handled, cmd := m.selectionKey(msg.String()); handled {
			return m, cmd
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+z":
			return m, tea.Suspend
		case "esc":
			cmd := m.dispatch(invocation{name: "back"})
			return m, cmd
		case "pgup":
			m.scrollBy(max(1, m.height-4))
			return m, nil
		case "pgdown":
			m.scrollBy(-max(1, m.height-4))
			return m, nil
		case "enter":
			return m.submit()
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.syncPalette(msg)
	if m.palette.entry == "skill" {
		cmd = tea.Batch(cmd, m.loadSkills(false))
	}
	return m, cmd
}

func (m Shell) submit() (tea.Model, tea.Cmd) {
	text := m.input.Value()
	command := strings.TrimSpace(text)
	if strings.HasPrefix(command, "/") {
		call, err := parseInvocation(command)
		if err != nil {
			m.report(err)
			return m, nil
		}
		cmd := m.submitCommand(call)
		return m, cmd
	}
	if _, pending := m.imageLoads[m.input.id]; pending {
		m.report(protocol.E("request_pending", "image load is unresolved; /image retries its original request"))
		return m, nil
	}
	if command == "" && len(m.input.images) == 0 && len(m.input.skills) == 0 {
		return m, nil
	}
	if m.activeQuestion != "" {
		q := m.answerDrafts[m.activeQuestion].info
		if q.State != "pending" {
			m.report(protocol.E("question_closed", "question is closed; local draft retained"))
			return m, nil
		}
		if len(text) > protocol.MaxAnswerBytes {
			m.report(protocol.E("invalid_params", "answer exceeds 1 MiB"))
			return m, nil
		}
		cmd, err := m.mutate("question.answer", protocol.AnswerQuestion{AgentID: m.agent, ID: q.ID, Answer: text, Attachments: m.input.imageIDs(), Skills: m.input.skillRefs()}, text)
		if err != nil {
			m.report(err)
		}
		return m, cmd
	}
	if len(text) > 2<<20 {
		m.report(protocol.E("invalid_params", "input exceeds 2 MiB"))
		return m, nil
	}
	cmd, err := m.mutate("agent.submit", protocol.Submit{AgentID: m.agent, Input: text, Attachments: m.input.imageIDs(), Skills: m.input.skillRefs()}, text)
	if err != nil {
		m.report(err)
		return m, nil
	}
	return m, cmd
}

func (m *Shell) scrollBy(up int) {
	if m.selection != nil {
		_, items := m.selectionChoices()
		m.selection.index = max(0, min(max(0, len(items)-1), m.selection.index-up))
		return
	}
	if m.help || m.showActivity || m.imagesPanel != nil || m.showQuestions || m.activeQuestion != "" && m.panel == nil {
		up = -up // Lists start at the top; conversation/log windows at the end.
	}
	m.scroll = max(0, min(10000, m.scroll+up))
}

func (m *Shell) submitCommand(call invocation) tea.Cmd {
	cmd, err := m.invoke(call)
	if err != nil {
		m.report(err)
		return nil
	}
	m.restoreCommandDraft()
	m.applyQuestionAction()
	m.applySkillPicker()
	return cmd
}

func safeText(text string) string { return process.DisplayText([]byte(text)) }

func singleLine(text string) string {
	return strings.NewReplacer("\n", `\n`, "\t", `\t`).Replace(safeText(text))
}

func (m Shell) View() tea.View {
	if m.width < 48 || m.height < 16 {
		v := tea.NewView(m.theme.base().Render(ansi.Truncate("Terminal too small: 48x16", m.width, "")))
		v.AltScreen = true
		return v
	}
	frame := m.layout()
	content := strings.Join(frame.body, "\n")
	if frame.images != "" {
		content += "\n" + m.theme.muted().Render(frame.images)
	}
	if frame.skills != "" {
		content += "\n" + m.theme.muted().Render(frame.skills)
	}
	content += "\n" + m.input.View()
	if len(frame.candidates) > 0 {
		content += "\n" + m.theme.muted().Render(strings.Join(frame.candidates, "\n"))
	}
	content += "\n" + m.theme.muted().Render(frame.status)
	v := tea.NewView(m.theme.base().Width(m.width).Height(m.height).Render(content))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.Cursor = m.input.Cursor()
	if v.Cursor != nil {
		v.Cursor.Y += len(frame.body)
		if frame.images != "" {
			v.Cursor.Y++
		}
		if frame.skills != "" {
			v.Cursor.Y++
		}
	}
	if m.selection != nil && m.commandDraft == nil {
		v.Cursor = nil // Navigation focus is the marked row, not the held draft.
	}
	return v
}

func (m *Shell) startWatch() tea.Cmd {
	if m.watchCancel != nil {
		m.watchCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.watchCancel = cancel
	m.generation++
	m.watching, m.retryPending = true, false
	return watchCmd(ctx, m.client, protocol.Cursor{AgentID: m.agent, After: m.cursor}, m.generation, m.emit)
}

func (m *Shell) reconnect() tea.Cmd {
	if m.ctx.Err() != nil || m.watching || m.retryPending || m.gap || m.attaching {
		return nil
	}
	delay := min(5*time.Second, 250*time.Millisecond*time.Duration(1<<min(m.retryAttempt, 5)))
	m.retryAttempt++
	m.retryPending = true
	return reconnectCmd(m.ctx, m.generation, delay)
}
