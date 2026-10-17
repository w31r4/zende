package render

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

// The UI observes a core-owned operation. Closing its panel or the whole TUI
// never cancels the stop or publication. Local drafts remain editable meanwhile.
type copyFlow struct {
	info    protocol.SessionCopyInfo
	local   bool // This editor supplied the accepted draft snapshot.
	follow  bool // Restoring input requires a copy or explicit status command.
	waiting bool
}

type copyInfoMsg struct {
	generation uint64
	info       protocol.SessionCopyInfo
	err        error
}

type restoredCopyDraft struct {
	draft    protocol.CopyDraft
	question protocol.QuestionInfo
	images   []protocol.Attachment
	skills   []protocol.ResourceInfo
}

type copyPreparedMsg struct {
	generation  uint64
	info        protocol.SessionCopyInfo
	fingerprint [32]byte
	active      string
	drafts      []restoredCopyDraft
	replaced    bool
	err         error
}

func validCopyInfo(info protocol.SessionCopyInfo, source string) bool {
	return validID(info.OperationID) && validID(source) && info.SourceAgent == source && validID(info.AgentID) && info.AgentID != source &&
		filepath.IsAbs(info.Cwd) && len(info.Cwd) <= 4096 && utf8.ValidString(info.Cwd) &&
		info.Drafts >= 0 && info.Drafts <= protocol.MaxCopyDrafts && len(info.Stage) > 0 && len(info.Stage) <= 64 &&
		((info.State == "stopping" || info.State == "copying") && info.SourceCursor == 0 && info.Cursor == 0 ||
			info.State == "completed" && info.Stage == "completed" && info.SourceCursor >= 0 && info.Cursor > info.SourceCursor)
}

func sameCopyIdentity(a, b protocol.SessionCopyInfo) bool {
	return a.OperationID == b.OperationID && a.SourceAgent == b.SourceAgent && a.AgentID == b.AgentID && a.Cwd == b.Cwd && a.Drafts == b.Drafts
}

func (m *Shell) copyCommand(name string, args []string) (tea.Cmd, error) {
	switch name {
	case "copy":
		if m.copyFlow != nil {
			return m.copyCommand("copy-status", nil)
		}
		if m.pending != nil {
			return nil, protocol.E("request_pending", "resolve the original request before copying")
		}
		path := ""
		if len(args) != 0 {
			path = args[0]
		}
		m.closePanel()
		m.selection = &selectionPanel{copy: true}
		return m.selectionQuery("directory", path, "", true), nil
	case "copy-confirm":
		p := m.selection
		if p == nil || !p.copy || p.kind != "copy-confirm" || p.waiting || !p.ready || !filepath.IsAbs(p.directory.Path) || m.copyFlow != nil {
			return nil, protocol.E("invalid_params", "choose a directory with /copy before confirming")
		}
		if m.pending != nil || len(m.imageLoads) > 0 || m.attaching || m.resizePending != nil {
			return nil, protocol.E("request_pending", "resolve pending input and terminal requests before copying")
		}
		// Run after command consumption, exactly like question/skill transitions.
		m.copyAction = true
	case "copy-status":
		id := ""
		if len(args) != 0 {
			id = args[0]
		}
		if m.copyFlow != nil {
			if id != "" && id != m.copyFlow.info.OperationID {
				return nil, protocol.E("request_pending", "another copy is retained by this editor")
			}
		} else {
			if !validID(id) {
				return nil, protocol.E("invalid_params", "use /copy-status OPERATION to reconnect a retained copy")
			}
			m.copyFlow = &copyFlow{info: protocol.SessionCopyInfo{OperationID: id}}
		}
		m.closePanel()
		m.selection = &selectionPanel{kind: "copy-progress", ready: true}
		m.copyFlow.follow = true
		return m.pollCopy(0), nil
	}
	return nil, nil
}

func (m *Shell) applyCopyAction() tea.Cmd {
	if !m.copyAction {
		return nil
	}
	m.copyAction = false
	p := m.selection
	if p == nil || p.kind != "copy-confirm" || !p.copy || !p.ready || p.waiting {
		m.report(protocol.E("invalid_state", "copy selection changed before confirmation"))
		return nil
	}
	drafts, _, err := m.copyDrafts()
	if err != nil {
		m.report(err)
		return nil
	}
	revision := p.base.Revision
	cmd, err := m.mutate("agent.copy", protocol.CopyAgent{AgentID: m.agent, Cwd: p.directory.Path, Confirm: true, ExpectedRevision: &revision, Drafts: drafts}, "")
	if err != nil {
		m.report(err)
		return nil
	}
	m.copyFlow = &copyFlow{local: true, follow: true, info: protocol.SessionCopyInfo{OperationID: m.pending.OperationID, SourceAgent: m.agent, Cwd: p.directory.Path, Drafts: len(drafts), Stage: "accepting"}}
	p.kind, p.index = "copy-progress", 0
	return cmd
}

func (m *Shell) rejectCopy() {
	m.copyFlow = nil
	m.copyGeneration++
	if m.selection != nil && m.selection.kind == "copy-progress" {
		m.closePanel()
	}
}

func (m *Shell) acceptCopy(accepted protocol.Accepted) tea.Cmd {
	if accepted.Copy == nil || !m.matchCopy(*accepted.Copy) {
		m.report(protocol.E("transport_unknown", "copy receipt did not match; /retry keeps the original request"))
		return nil
	}
	m.copyFlow.info = *accepted.Copy
	m.pending = nil
	m.sentText, m.sentDraft, m.sentQuestion = "", draftStamp{}, protocol.QuestionInfo{}
	m.report(nil)
	return m.pollCopy(0)
}

func (m Shell) matchCopy(info protocol.SessionCopyInfo) bool {
	if !validCopyInfo(info, info.SourceAgent) || info.SourceAgent != m.agent && info.AgentID != m.agent || m.copyFlow == nil {
		return false
	}
	old := m.copyFlow.info
	if old.AgentID != "" {
		return sameCopyIdentity(old, info)
	}
	return info.OperationID == old.OperationID && (old.SourceAgent == "" || info.SourceAgent == old.SourceAgent) &&
		(old.Cwd == "" || info.Cwd == old.Cwd) && (old.Stage != "accepting" || old.Drafts == info.Drafts)
}

// Source events expose retained progress, but historical replay never moves an
// editor by itself. /copy-status explicitly reconnects after detaching.
func (m *Shell) projectCopyEvent(event protocol.Event) tea.Cmd {
	if event.Kind != "session.copy.started" && event.Kind != "session.copy.progress" && event.Kind != "session.copy.completed" {
		return nil
	}
	var info protocol.SessionCopyInfo
	if decodePayload(event, &info) != nil || !validCopyInfo(info, info.SourceAgent) || info.SourceAgent != m.agent && info.AgentID != m.agent {
		m.issues.raise("copy", protocol.E("invalid_state", "copy event has invalid source or progress"))
		return nil
	}
	if info.AgentID == m.agent && m.restoredCopy == info.OperationID {
		return nil
	}
	if info.AgentID == m.agent && (m.copyFlow == nil || m.copyFlow.info.OperationID != info.OperationID) {
		return nil // An old receipt must not repopulate already-submitted input.
	}
	if m.copyFlow == nil {
		m.copyFlow = &copyFlow{info: info}
	} else if !m.matchCopy(info) {
		m.issues.raise("copy", protocol.E("invalid_state", "copy operation identity changed"))
		return nil
	} else if m.copyFlow.info.State != "completed" {
		m.copyFlow.info = info
	}
	if m.pending != nil && m.pending.Method == "agent.copy" && m.pending.OperationID == info.OperationID {
		m.pending = nil // Durable event is also proof of the original acceptance.
		m.report(nil)
	}
	return m.pollCopy(0)
}

func (m *Shell) pollCopy(delay time.Duration) tea.Cmd {
	if m.copyFlow == nil || m.copyFlow.waiting {
		return nil
	}
	m.copyFlow.waiting = true
	m.copyGeneration++
	generation, ctx, client, id := m.copyGeneration, m.ctx, m.client, m.copyFlow.info.OperationID
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		msg := copyInfoMsg{generation: generation}
		msg.err = queryUI(ctx, client, "agent.copy.inspect", struct {
			ID string `json:"operation_id"`
		}{id}, &msg.info)
		return msg
	}
}

func (m *Shell) updateCopy(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case copyInfoMsg:
		if m.copyFlow == nil || msg.generation != m.copyGeneration || !m.copyFlow.waiting {
			return true, nil
		}
		m.copyFlow.waiting = false
		unrelated := msg.err == nil && !m.matchCopy(msg.info)
		if msg.err == nil && !m.matchCopy(msg.info) {
			msg.err = protocol.E("invalid_state", "copy status belongs to another operation or source")
		}
		if msg.err != nil {
			var known *protocol.Error
			if (unrelated || errors.As(msg.err, &known) && known.Data.Kind == "not_found") && !m.copyFlow.local && m.copyFlow.info.AgentID == "" {
				m.rejectCopy() // A mistaken status ID must not freeze a healthy editor.
				m.report(msg.err)
				return true, nil
			}
			m.issues.raise("copy", msg.err)
			return true, m.pollCopy(2 * time.Second)
		}
		if m.copyFlow.info.State != "completed" {
			m.copyFlow.info = msg.info
		}
		if m.pending != nil && m.pending.Method == "agent.copy" && m.pending.OperationID == msg.info.OperationID {
			m.pending = nil
			m.report(nil)
		}
		if m.copyFlow.info.Failure != nil {
			m.issues.raise("copy", m.copyFlow.info.Failure)
		} else if m.copyFlow.info.State != "completed" || !m.copyFlow.follow {
			m.issues.resolve("copy")
		}
		if m.copyFlow.info.State == "completed" {
			if !m.copyFlow.follow {
				return true, nil
			}
			return true, m.prepareCopy()
		}
		return true, m.pollCopy(500 * time.Millisecond)
	case copyPreparedMsg:
		if m.copyFlow == nil || msg.generation != m.copyGeneration || !m.copyFlow.waiting {
			return true, nil
		}
		m.copyFlow.waiting = false
		if msg.err != nil {
			m.issues.raise("copy", msg.err)
			return true, m.pollCopy(2 * time.Second)
		}
		if m.sessionOpening != nil {
			return true, m.pollCopy(500 * time.Millisecond) // An explicit session navigation wins over auto-follow.
		}
		_, fingerprint, err := m.copyDrafts()
		if err != nil {
			m.issues.raise("copy", err)
			return true, m.pollCopy(2 * time.Second)
		}
		if fingerprint != msg.fingerprint || m.pending != nil || len(m.imageLoads) > 0 || m.attaching || m.resizePending != nil {
			return true, m.pollCopy(500 * time.Millisecond)
		}
		cmd, err := m.openCopy(msg)
		if err != nil {
			m.issues.raise("copy", err)
			return true, m.pollCopy(2 * time.Second)
		}
		return true, cmd
	}
	return false, nil
}

func nonemptyCopyDraft(draft protocol.CopyDraft) bool {
	return draft.Text != "" || len(draft.Attachments) > 0 || len(draft.Skills) > 0
}

// Includes the held main editor and every retained answer editor. The temporary
// slash/skill command editor is not user input. Sorting makes the read fence stable.
func (m Shell) copyDrafts() ([]protocol.CopyDraft, [32]byte, error) {
	main := m.editDraft()
	if m.activeQuestion != "" {
		main = m.mainDraft
	}
	if main == nil {
		return nil, [32]byte{}, protocol.E("invalid_state", "main draft is unavailable; current editor retained")
	}
	from := func(c *Composer, q protocol.QuestionInfo) protocol.CopyDraft {
		return protocol.CopyDraft{ID: c.id, QuestionID: q.ID, QuestionAgent: q.AgentID, Text: c.Value(), Attachments: c.imageIDs(), Skills: c.skillRefs()}
	}
	drafts := []protocol.CopyDraft{from(main, protocol.QuestionInfo{})}
	ids := make([]string, 0, len(m.answerDrafts))
	for id := range m.answerDrafts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		answer := m.answerDrafts[id]
		c := &answer.input
		if id == m.activeQuestion {
			c = m.editDraft()
		}
		draft := from(c, answer.info)
		if nonemptyCopyDraft(draft) || id == m.activeQuestion {
			drafts = append(drafts, draft)
		}
	}
	raw, err := json.Marshal(struct {
		Drafts []protocol.CopyDraft `json:"drafts"`
		Active string               `json:"active"`
	}{drafts, m.activeQuestion})
	if err != nil || len(drafts) > protocol.MaxCopyDrafts || len(raw) > protocol.MaxCopyDraftBytes {
		return nil, [32]byte{}, protocol.E("input_limit", "copy drafts exceed 17 editors or 6 MiB; local input retained")
	}
	return drafts, sha256.Sum256(raw), nil
}

func (m *Shell) prepareCopy() tea.Cmd {
	if m.pending != nil || len(m.imageLoads) > 0 || m.attaching || m.resizePending != nil {
		return m.pollCopy(500 * time.Millisecond)
	}
	drafts, fingerprint, err := m.copyDrafts()
	if err != nil {
		m.issues.raise("copy", err)
		return m.pollCopy(2 * time.Second)
	}
	m.copyFlow.waiting = true
	m.copyGeneration++
	generation, ctx, client := m.copyGeneration, m.ctx, m.client
	info, local, active := m.copyFlow.info, m.copyFlow.local, m.activeQuestion
	viewingTarget := info.AgentID == m.agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		msg := copyPreparedMsg{generation: generation, info: info, fingerprint: fingerprint, active: active}
		var retained protocol.SessionCopyDrafts
		msg.err = queryUI(ctx, client, "agent.copy.drafts", protocol.CopyDraftQuery{ID: info.OperationID}, &retained)
		if msg.err != nil {
			return msg
		}
		if !sameCopyIdentity(retained.Copy, info) || retained.Copy.State != "completed" || len(retained.Drafts) != info.Drafts {
			msg.err = protocol.E("invalid_state", "retained copy drafts did not match the published operation")
			return msg
		}
		current := drafts
		if !local {
			current = nil
			for _, draft := range drafts {
				if nonemptyCopyDraft(draft) {
					current = append(current, draft)
				}
			}
		}
		var newer protocol.SessionCopyDrafts
		if viewingTarget {
			// Reopening the target uses already-owned references, not source IDs.
			newer.Drafts = current
		} else if len(current) > 0 {
			msg.err = queryUI(ctx, client, "agent.copy.drafts", protocol.CopyDraftQuery{ID: info.OperationID, Drafts: current}, &newer)
			if msg.err != nil {
				return msg
			}
			if !sameCopyIdentity(newer.Copy, info) || newer.Copy.State != "completed" || len(newer.Drafts) != len(current) {
				msg.err = protocol.E("invalid_state", "updated copy drafts did not match their read request")
				return msg
			}
			for i, old := range current {
				next := newer.Drafts[i]
				owner := old.QuestionAgent
				if old.QuestionID != "" && owner == "" {
					owner = info.SourceAgent
				}
				if next.ID != old.ID || next.QuestionID != old.QuestionID || next.QuestionAgent != owner || next.Text != old.Text || !reflect.DeepEqual(next.Skills, old.Skills) || len(next.Attachments) != len(old.Attachments) {
					msg.err = protocol.E("invalid_state", "updated draft contents changed during remapping")
					return msg
				}
			}
		}
		selected := retained.Drafts
		if local {
			selected = newer.Drafts
		} else {
			for _, draft := range newer.Drafts {
				found := false
				for i, old := range selected {
					if old.QuestionID == draft.QuestionID {
						previous, next := old, draft
						previous.ID, next.ID = "", ""
						msg.replaced = msg.replaced || !reflect.DeepEqual(previous, next)
						selected[i], found = draft, true
						break
					}
				}
				if !found {
					selected = append(selected, draft)
				}
			}
		}
		msg.drafts, msg.err = readCopyDraftMetadata(ctx, client, info, selected)
		return msg
	}
}

func readCopyDraftMetadata(ctx context.Context, client Client, info protocol.SessionCopyInfo, drafts []protocol.CopyDraft) ([]restoredCopyDraft, error) {
	if len(drafts) > protocol.MaxCopyDrafts {
		return nil, protocol.E("draft_limit", "combined copy drafts exceed 17 editors; local and retained drafts are unchanged")
	}
	raw, err := json.Marshal(drafts)
	if err != nil || len(raw) > protocol.MaxCopyDraftBytes {
		return nil, protocol.E("input_limit", "combined copy drafts exceed 6 MiB; local and retained drafts are unchanged")
	}
	ids, questions := map[string]bool{}, map[string]bool{}
	images := map[string]protocol.Attachment{}
	skills := map[protocol.ResourceRef]protocol.ResourceInfo{}
	result := make([]restoredCopyDraft, 0, len(drafts))
	for _, draft := range drafts {
		if !validID(draft.ID) || ids[draft.ID] || questions[draft.QuestionID] || !utf8.ValidString(draft.Text) || len(draft.Attachments) > protocol.MaxInputImages || len(draft.Skills) > protocol.MaxInputSkills {
			return nil, protocol.E("invalid_state", "copy draft identity, input or reference limit is invalid")
		}
		ids[draft.ID], questions[draft.QuestionID] = true, true
		next := restoredCopyDraft{draft: draft}
		if draft.QuestionID != "" {
			if !validID(draft.QuestionID) || !validID(draft.QuestionAgent) {
				return nil, protocol.E("invalid_state", "copied answer draft has no source question identity")
			}
			var question protocol.Question
			if err := queryUI(ctx, client, "question.inspect", protocol.QuestionTarget{AgentID: draft.QuestionAgent, ID: draft.QuestionID}, &question); err != nil {
				return nil, err
			}
			if !validQuestion(question.QuestionInfo, draft.QuestionAgent) || question.ID != draft.QuestionID || question.State == "pending" && draft.QuestionAgent != info.AgentID {
				return nil, protocol.E("invalid_state", "copied answer draft's source question is not closed")
			}
			next.question = question.QuestionInfo
		} else if draft.QuestionAgent != "" {
			return nil, protocol.E("invalid_state", "main copy draft must not name a source question Agent")
		}
		for _, id := range draft.Attachments {
			image, found := images[id]
			if !found {
				if err := queryUI(ctx, client, "attachment.inspect", protocol.AttachmentTarget{AgentID: info.AgentID, ID: id}, &image); err != nil {
					return nil, err
				}
				if !validImageInfo(image, info.AgentID) || image.ID != id {
					return nil, protocol.E("invalid_state", "copied image belongs to another session or reference")
				}
				images[id] = image
			}
			next.images = append(next.images, image)
		}
		for _, ref := range draft.Skills {
			skill, found := skills[ref]
			if !found {
				var content protocol.ResourceContent
				if err := queryUI(ctx, client, "resource.read", protocol.ResourceRead{AgentID: info.AgentID, ID: ref.ID, Version: ref.Version}, &content); err != nil {
					return nil, err
				}
				skill = content.ResourceInfo
				if !validSkillInfo(skill) || skill.ID != ref.ID || skill.Version != ref.Version {
					return nil, protocol.E("invalid_state", "copied skill does not match its selected version")
				}
				skills[ref] = skill
			}
			next.skills = append(next.skills, skill)
		}
		result = append(result, next)
	}
	return result, nil
}

func (m *Shell) openCopy(msg copyPreparedMsg) (tea.Cmd, error) {
	if !m.matchCopy(msg.info) || msg.info.State != "completed" {
		return nil, protocol.E("invalid_state", "copy is not published; current editor retained")
	}
	switching := m.agent != msg.info.AgentID
	next := *m
	if switching {
		next = m.newSession(msg.info.AgentID)
	}
	// Build every editor before replacing anything in the current Shell.
	discard := func() {
		if switching {
			next.sessionCancel()
		}
	}
	next.answerDrafts = make(map[string]*answerDraft)
	main := NewComposer()
	main.SetWidth(m.width)
	main.setTheme(m.theme)
	for _, saved := range msg.drafts {
		input := NewComposer() // Fresh editor IDs reject late source image loads.
		input.SetWidth(m.width)
		input.setTheme(m.theme)
		input.SetValue(saved.draft.Text)
		if input.Value() != saved.draft.Text {
			discard()
			return nil, protocol.E("input_limit", "copied text does not fit its editor; source and retained draft are unchanged")
		}
		for _, image := range saved.images {
			if err := input.addImage(image); err != nil {
				discard()
				return nil, err
			}
		}
		for _, skill := range saved.skills {
			if err := input.addSkill(skill); err != nil {
				discard()
				return nil, err
			}
		}
		if saved.draft.QuestionID == "" {
			main = input
		} else {
			question := saved.question
			if !switching && question.AgentID == m.agent {
				if previous := m.answerDrafts[question.ID]; previous != nil {
					question = previous.info // Live close/snapshot state wins over the read.
				}
			}
			next.answerDrafts[saved.draft.QuestionID] = &answerDraft{info: question, input: input}
		}
	}
	next.input, next.mainDraft, next.activeQuestion = main, nil, ""
	next.commandDraft, next.palette = nil, commandPalette{}
	next.copyFlow, next.copyGeneration, next.restoredCopy = nil, m.copyGeneration+1, msg.info.OperationID
	if next.selection != nil && next.selection.kind == "copy-progress" {
		next.closePanel()
	}
	if answer := next.answerDrafts[msg.active]; answer != nil {
		next.mainDraft, next.activeQuestion, next.input = &main, msg.active, answer.input
	}
	if msg.replaced {
		next.issues.raise("copy-drafts", protocol.E("drafts_retained", "newer local drafts kept; originals remain in agent copy-drafts "+msg.info.OperationID))
	}
	if switching {
		merged, err := next.mergeCachedSessionDrafts()
		if err != nil {
			discard()
			return nil, err
		}
		if merged {
			next.issues.raise("copy-drafts", protocol.E("drafts_retained", "cached local drafts kept; originals remain in agent copy-drafts "+msg.info.OperationID))
		}
	}
	if !switching {
		next.issues.resolve("copy")
		*m = next
		return nil, nil
	}
	m.releaseSession() // Client work only; the core and copied Agent keep running.
	*m = next
	return m.Init(), nil
}

func (m Shell) copyChoices() (string, []selectionRow) {
	if m.copyFlow == nil {
		return "Copy", nil
	}
	info := m.copyFlow.info
	title := "Copy"
	if info.Cwd != "" {
		title += m.theme.separator() + info.Cwd
	}
	stage := info.Stage
	if stage == "" {
		stage = "connecting"
	}
	if stage == "completed" {
		stage = "restoring drafts"
	}
	return title, []selectionRow{{text: stage}, {text: info.OperationID}}
}
