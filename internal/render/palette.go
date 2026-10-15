package render

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type commandPalette struct {
	query string
	start int // Byte offset in the draft, not terminal columns.
	entry string
	items []paletteItem
	index int
}

type paletteItem struct {
	command
	args  []string
	title string
}

func (c Composer) atEnd() bool {
	lines := strings.Split(c.Value(), "\n")
	info := c.LineInfo()
	return c.Line() == len(lines)-1 && info.StartColumn+info.ColumnOffset == utf8.RuneCountInString(lines[len(lines)-1])
}

func commandToken(c Composer) (query string, start int, ok bool) {
	if !c.atEnd() {
		return "", 0, false
	}
	text := c.Value()
	start = strings.LastIndexFunc(text, unicode.IsSpace)
	if start >= 0 {
		_, size := utf8.DecodeRuneInString(text[start:])
		start += size
	} else {
		start = 0
	}
	// Arguments of a command are not another command palette.
	if start > 0 && strings.HasPrefix(strings.TrimSpace(text[:start]), "/") {
		return "", 0, false
	}
	token := text[start:]
	if !strings.HasPrefix(token, "/") {
		return "", 0, false
	}
	return token[1:], start, true
}

func paletteQuery(c Composer) (query string, start int, entry string, ok bool) {
	text := c.Value()
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	for _, name := range []string{"commentary", "image-use", "image-remove", "answer", "answer-clear"} {
		if rest, match := strings.CutPrefix(trimmed, "/"+name); match && len(rest) > 0 && c.atEnd() {
			r, _ := utf8.DecodeRuneInString(rest)
			if unicode.IsSpace(r) {
				return strings.TrimSpace(rest), len(text) - len(trimmed), name, true
			}
		}
	}
	query, start, ok = commandToken(c)
	return query, start, "", ok
}

func (m *Shell) syncPalette(msg tea.Msg) {
	if _, pasted := msg.(tea.PasteMsg); pasted {
		m.palette = commandPalette{} // Pasting text never chooses a command.
		return
	}
	if _, key := msg.(tea.KeyPressMsg); !key {
		return
	}
	m.syncPaletteKey()
}

func (m *Shell) syncPaletteKey() {
	query, start, entry, ok := paletteQuery(m.input)
	if !ok {
		m.palette = commandPalette{}
		return
	}
	if query == m.palette.query && start == m.palette.start && entry == m.palette.entry && len(m.palette.items) > 0 {
		return
	}
	p := commandPalette{query: query, start: start, entry: entry}
	if entry == "answer" || entry == "answer-clear" {
		c, _ := lookupCommand(entry)
		for _, q := range m.questionOptions() {
			if entry == "answer-clear" && m.answerDrafts[q.ID] == nil {
				continue
			}
			if strings.Contains(strings.ToLower(q.ID+" "+q.Prompt), strings.ToLower(query)) {
				p.items = append(p.items, paletteItem{command: c, args: []string{q.ID}, title: ansi.Truncate(questionTitle(q), 120, "...")})
			}
		}
		m.palette = p
		return
	}
	if entry == "commentary" {
		c, _ := lookupCommand("commentary")
		for _, e := range m.transcript.Entries {
			if e.Kind != "assistant" || e.Category != "commentary" || e.Hidden || e.Key == "" || !strings.Contains(strings.ToLower(e.Key+" "+e.Text), strings.ToLower(query)) {
				continue
			}
			title := ansi.Truncate(strings.Join(strings.Fields(safeText(e.Text)), " "), 120, "...")
			if title == "" {
				title = ansi.Truncate(safeText(e.Key), 120, "...")
			}
			p.items = append(p.items, paletteItem{command: c, args: []string{e.Key}, title: title})
		}
		m.palette = p
		return
	}
	if entry == "image-use" || entry == "image-remove" {
		c, _ := lookupCommand(entry)
		images := m.editDraft().images
		if entry == "image-use" && m.imagesPanel != nil {
			images = m.imagesPanel.page.Items
		}
		for _, image := range images {
			if strings.Contains(strings.ToLower(image.ID+" "+image.Name), strings.ToLower(query)) {
				p.items = append(p.items, paletteItem{command: c, args: []string{image.ID}, title: imageTitle(image, m.theme.separator())})
			}
		}
		m.palette = p
		return
	}
	for _, c := range commands {
		match := strings.HasPrefix(c.name, query)
		for _, alias := range c.aliases {
			match = match || strings.HasPrefix(alias, query)
		}
		if match {
			p.items = append(p.items, paletteItem{command: c})
			if c.name == query {
				p.index = len(p.items) - 1
			}
			for _, alias := range c.aliases {
				if alias == query {
					p.index = len(p.items) - 1
				}
			}
		}
	}
	m.palette = p
}

func (m Shell) paletteActive() bool {
	query, start, entry, ok := paletteQuery(m.input)
	return ok && query == m.palette.query && start == m.palette.start && entry == m.palette.entry && len(m.palette.items) > 0
}

func (m *Shell) paletteKey(key string) (bool, tea.Cmd) {
	if !m.paletteActive() {
		return false, nil
	}
	switch key {
	case "up":
		m.palette.index = (m.palette.index + len(m.palette.items) - 1) % len(m.palette.items)
	case "down":
		m.palette.index = (m.palette.index + 1) % len(m.palette.items)
	case "tab":
		return true, m.chooseCandidate(m.palette.index, false)
	case "enter":
		return true, m.chooseCandidate(m.palette.index, true)
	case "esc":
		m.palette = commandPalette{}
		if m.commandDraft != nil {
			m.restoreCommandDraft()
		}
	default:
		return false, nil
	}
	return true, nil
}

func (m *Shell) chooseCandidate(index int, execute bool) tea.Cmd {
	if !m.paletteActive() || index < 0 || index >= len(m.palette.items) {
		return nil
	}
	c := m.palette.items[index]
	if m.palette.start > 0 && m.commandDraft == nil {
		// The command is temporary. The preceding multiline draft stays in its
		// own editor, including whitespace; it is never submitted as arguments.
		draft := NewComposer()
		draft.SetWidth(m.width)
		draft.setTheme(m.theme)
		draft.SetValue(m.input.Value()[:m.palette.start])
		draft.id, draft.revision = m.input.id, m.input.revision
		draft.images = append([]protocol.Attachment(nil), m.input.images...)
		m.commandDraft = &draft
		m.input = NewComposer()
		m.input.SetWidth(m.width)
		m.input.setTheme(m.theme)
	}
	m.input.SetValue("/" + c.name)
	for _, arg := range c.args {
		m.input.InsertString(" '" + strings.ReplaceAll(arg, "'", "'\\''") + "'")
	}
	m.palette = commandPalette{}
	if execute {
		return m.submitCommand(invocation{name: c.name, args: c.args})
	}
	if c.usage != "" && len(c.args) == 0 {
		m.input.InsertString(" ")
		m.syncPalette(tea.KeyPressMsg{})
	}
	return nil
}

func (m *Shell) restoreCommandDraft() {
	if m.commandDraft != nil {
		m.input = *m.commandDraft
		m.commandDraft = nil
	} else {
		m.input.ResetText()
	}
	m.palette = commandPalette{}
}
