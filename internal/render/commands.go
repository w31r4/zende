package render

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

// Commands are the shared entry points for typed commands, candidates and mouse
// targets. A target carries arguments, never synthetic keystrokes or RPC names.
type invocation struct {
	name string
	args []string
}

type command struct {
	name, usage, description string
	aliases                  []string
	maxArgs                  int
}

var commands = []command{
	{name: "help", description: "Commands"},
	{name: "background", description: "Background work"},
	{name: "model", usage: "[CONNECTION] [MODEL]", description: "Choose configured model", maxArgs: 2},
	{name: "effort", usage: "[VALUE]", description: "Choose reasoning effort", maxArgs: 1},
	{name: "select", usage: "[strict|public]", description: "Confirm current selection", maxArgs: 1},
	{name: "cd", usage: "[PATH]", description: "Browse working directory", maxArgs: 1},
	{name: "copy", usage: "[PATH]", description: "Copy session to a directory", maxArgs: 1},
	{name: "copy-confirm", description: "Stop source tree and copy session"},
	{name: "copy-status", usage: "[OPERATION]", description: "Resume session copy", maxArgs: 1},
	{name: "git", description: "Working tree status"},
	{name: "terminal", usage: "[PROCESS]", description: "Select terminal", maxArgs: 1},
	{name: "logs", usage: "[PROCESS] [stdout|stderr|pty]", description: "Read process output", maxArgs: 2},
	{name: "commentary", usage: "[ENTRY]", description: "Fold commentary", maxArgs: 1},
	{name: "issues", description: "Warnings and errors"},
	{name: "questions", description: "Pending questions / drafts"},
	{name: "answer", usage: "[QUESTION]", description: "Answer a question", maxArgs: 1},
	{name: "answer-clear", usage: "[QUESTION]", description: "Discard local answer draft", maxArgs: 1},
	{name: "image", usage: "[PATH]", description: "Import image / retry", maxArgs: 1},
	{name: "images", description: "Saved images"},
	{name: "image-use", usage: "IMAGE", description: "Select saved image", maxArgs: 1},
	{name: "image-remove", usage: "IMAGE", description: "Remove draft image", maxArgs: 1},
	{name: "skill", usage: "[NAME|RESOURCE] [VERSION]", description: "Select skill / refresh list", maxArgs: 2},
	{name: "skill-remove", usage: "[NAME|RESOURCE]", description: "Remove draft skill", maxArgs: 1},
	{name: "refresh", description: "Refresh connection"},
	{name: "retry", description: "Retry original request"},
	{name: "stop", description: "Stop current work"},
	{name: "resume", description: "Resume Agent"},
	{name: "recover", description: "Recover interrupted work"},
	{name: "compact", description: "Summarize earlier context"},
	{name: "attach", usage: "[PROCESS]", description: "Interact with PTY", maxArgs: 1},
	{name: "resize", usage: "[COLS ROWS]", description: "Resize selected PTY", maxArgs: 2},
	{name: "next", description: "Next page"},
	{name: "back", description: "Return to conversation"},
	{name: "detach", description: "Close UI, keep work running", aliases: []string{"quit"}},
}

func lookupCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
		for _, alias := range c.aliases {
			if alias == name {
				return c, true
			}
		}
	}
	return command{}, false
}

func parseInvocation(text string) (invocation, error) {
	// Shell-like quoting only. There is no interpolation or shell execution.
	var fields []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range text {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, started = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case unicode.IsSpace(r):
			if started {
				fields = append(fields, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return invocation{}, protocol.E("invalid_params", "unfinished command quote or escape")
	}
	if started {
		fields = append(fields, word.String())
	}
	if len(fields) == 0 {
		return invocation{}, protocol.E("invalid_params", "empty command")
	}
	return invocation{strings.TrimPrefix(fields[0], "/"), fields[1:]}, nil
}

func (m *Shell) invoke(call invocation) (tea.Cmd, error) {
	c, ok := lookupCommand(call.name)
	if !ok {
		return nil, protocol.E("invalid_params", "unknown command; /help")
	}
	if len(call.args) > c.maxArgs {
		return nil, protocol.E("invalid_params", strings.TrimSpace("use /"+c.name+" "+c.usage))
	}
	switch c.name {
	case "detach":
		return tea.Quit, nil
	case "help":
		m.closePanel()
		m.help = true
	case "background":
		m.closePanel()
		m.showActivity = true
		return m.startActivity(), nil
	case "issues":
		m.issues.open = !m.issues.open
		m.issues.manual = m.issues.open
	case "image", "images", "image-use", "image-remove":
		return m.imageCommand(c.name, call.args)
	case "skill", "skill-remove":
		return m.skillCommand(c.name, call.args)
	case "questions", "answer", "answer-clear":
		return nil, m.questionCommand(c.name, call.args)
	case "model", "effort", "select", "cd", "git":
		return m.selectionCommand(c.name, call.args)
	case "copy", "copy-confirm", "copy-status":
		return m.copyCommand(c.name, call.args)
	case "next":
		if m.selection != nil {
			return m.selectionNext()
		}
		if m.imagesPanel != nil {
			if m.imagesPanel.waiting || m.imagesPanel.page.Next == "" {
				return nil, protocol.E("not_found", "no next image page")
			}
			return m.openImages(m.imagesPanel.page.Next), nil
		}
		return m.runProcessCommand(c.name, call.args)
	case "commentary":
		if len(call.args) == 0 {
			m.foldCommentary = !m.foldCommentary
			m.commentaryOverrides = nil
		} else if !m.toggleCommentary(call.args[0]) {
			return nil, protocol.E("not_found", "commentary entry is outside the display window")
		}
		m.scroll = 0
	case "back":
		selection := m.selection != nil
		m.closePanel()
		m.issues.open, m.issues.manual = false, false
		if !selection {
			m.questionAction = &questionAction{name: "back"}
		}
	case "refresh":
		m.report(nil)
		m.gap = false
		if !m.refreshing {
			m.refreshing = true
			var poll tea.Cmd
			if m.panel != nil && m.panel.kind == "log" && !m.panel.waiting {
				poll = pollProcessCmd(m.ctx, m.panel.generation, 0)
			}
			return tea.Batch(snapshotCmd(m.ctx, m.client, m.agent), poll, m.syncGit(true)), nil
		}
	case "retry":
		if m.pending == nil {
			if m.copyFlow != nil {
				return m.pollCopy(0), nil
			}
			return nil, protocol.E("invalid_params", "no pending request")
		}
		return mutationCmd(m.ctx, m.client, *m.pending), nil
	case "stop", "resume":
		return m.mutate("agent."+c.name, protocol.Target{AgentID: m.agent}, "")
	case "recover":
		return m.mutate("agent.recover", protocol.RecoveryTarget{AgentID: m.agent}, "")
	case "compact":
		return m.mutate("context.compact", protocol.CompactContext{AgentID: m.agent}, "")
	case "terminal", "logs", "attach", "resize":
		return m.runProcessCommand(c.name, call.args)
	}
	return nil, nil
}

// dispatch keeps an existing draft intact. Command-line consumption is handled
// separately, only after local validation succeeds.
func (m *Shell) dispatch(call invocation) tea.Cmd {
	cmd, err := m.invoke(call)
	if err != nil {
		m.report(err)
	} else {
		m.applyQuestionAction()
		m.applySkillPicker()
		cmd = tea.Batch(cmd, m.applyCopyAction())
	}
	return cmd
}

func (m *Shell) closePanel() {
	m.help, m.showActivity, m.panel, m.scroll = false, false, nil, 0
	m.imagesPanel = nil
	m.selection = nil
	m.selectionGeneration++
	m.issues.resolve("selection")
	m.showQuestions = false
	m.imagesGeneration++
	m.processGeneration++
	m.issues.resolve("process")
}

func (m *Shell) mutate(method string, params any, sentText string) (tea.Cmd, error) {
	if m.pending != nil {
		return nil, protocol.E("request_pending", "original request is unresolved; /retry keeps its ID")
	}
	if m.copyFlow != nil && method != "agent.copy" {
		return nil, protocol.E("copy_pending", "source session is retained; continue when its copy is ready")
	}
	req, err := protocol.NewRequest(method, protocol.NewID("ui"), params)
	if err != nil {
		return nil, err
	}
	m.pending, m.sentText, m.err = &req, sentText, nil
	m.sentDraft = draftStamp{}
	m.sentQuestion = protocol.QuestionInfo{}
	if method == "agent.submit" || method == "question.answer" {
		m.sentDraft = m.input.stamp()
	}
	if method == "question.answer" {
		m.sentQuestion = m.answerDrafts[m.activeQuestion].info
	}
	m.issues.resolve("request")
	return mutationCmd(m.ctx, m.client, req), nil
}

func (m *Shell) toggleCommentary(key string) bool {
	kept := make(map[string]bool)
	found := false
	for _, entry := range m.transcript.Entries {
		if entry.Kind != "assistant" || entry.Category != "commentary" || entry.Hidden || entry.Key == "" {
			continue
		}
		value, overridden := m.commentaryOverrides[entry.Key]
		if entry.Key == key {
			if !overridden {
				value = m.foldCommentary
			}
			value, overridden, found = !value, true, true
		}
		if overridden {
			kept[entry.Key] = value
		}
	}
	m.commentaryOverrides = kept // At most the bounded projection's entries.
	return found
}
