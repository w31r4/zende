package render

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type hitTarget struct {
	x, y, width int
	call        invocation
	candidate   int // -1 for a direct action, otherwise a visible candidate.
}

type frame struct {
	body, candidates []string
	status, images   string
	targets          []hitTarget
}

// One pure layout supplies both rendering and cell-based hit testing. Wrapping,
// folding, scrolling and resize therefore cannot detach an action from its row.
func (m Shell) layout() frame {
	f := frame{}
	if m.width < 48 || m.height < 16 {
		return f
	}
	if m.paletteActive() {
		count := min(4, len(m.palette.items))
		start := max(0, m.palette.index-count+1)
		for i := start; i < len(m.palette.items) && len(f.candidates) < count; i++ {
			c := m.palette.items[i]
			mark := "  "
			if i == m.palette.index {
				mark = "> "
			}
			text := strings.TrimSpace("/"+c.name+" "+c.usage) + m.theme.separator() + c.description
			if len(c.args) > 0 {
				text = c.title
			}
			line := ansi.Truncate(mark+text, m.width, "...")
			y := m.height - 1 - count + len(f.candidates)
			f.targets = append(f.targets, hitTarget{x: 0, y: y, width: ansi.StringWidth(line), call: invocation{name: c.name, args: c.args}, candidate: i})
			f.candidates = append(f.candidates, line)
		}
	}
	issues := m.issues.lines(m.width, min(5, m.height/3))
	rows := m.height - 3 - len(f.candidates) - len(issues)
	f.images = m.imageStrip()
	if f.images != "" {
		rows--
	}
	switch {
	case m.help:
		start := min(m.scroll, max(0, len(commands)-rows))
		for i := start; i < len(commands) && len(f.body) < rows; i++ {
			c := commands[i]
			// Help is reference text, not a second wall of clickable buttons.
			f.body = append(f.body, strings.TrimSpace("/"+c.name+" "+c.usage)+m.theme.separator()+c.description)
		}
	case m.showActivity:
		f.body = m.activityLines(m.width, rows)
	case m.imagesPanel != nil:
		m.imageRows(&f, rows)
	case m.panel != nil:
		f.body = m.panel.renderLines(m.width, rows, m.scroll, m.theme.separator(), false)
	case m.showQuestions || m.activeQuestion != "":
		m.questionRows(&f, rows)
	default:
		for y, row := range m.transcript.viewRows(m.width, rows, m.scroll, m.foldCommentary, m.commentaryOverrides, m.theme.separator()) {
			f.body = append(f.body, row.text)
			if row.commentaryKey != "" {
				f.targets = append(f.targets, hitTarget{x: 0, y: y, width: ansi.StringWidth(row.text), call: invocation{name: "commentary", args: []string{row.commentaryKey}}, candidate: -1})
			}
		}
	}
	if len(f.body) > rows {
		f.body = f.body[:rows]
	}
	for len(f.body) < rows {
		f.body = append(f.body, "")
	}
	f.body = append(f.body, issues...)
	if f.images != "" {
		f.targets = append(f.targets, hitTarget{x: 0, y: len(f.body), width: ansi.StringWidth(f.images), call: invocation{name: "images"}, candidate: -1})
	}
	for i := range f.body {
		f.body[i] = ansi.Truncate(f.body[i], m.width, "...")
	}
	var targets []hitTarget
	f.status, targets = m.statusProjection()
	f.targets = append(f.targets, targets...)
	return f
}

func (m *Shell) click(x, y int) tea.Cmd {
	if x < 0 || x >= m.width || y < 0 || y >= m.height {
		return nil
	}
	for _, target := range m.layout().targets {
		if y != target.y || x < target.x || x >= target.x+target.width {
			continue
		}
		if target.candidate >= 0 {
			return m.chooseCandidate(target.candidate, true)
		}
		return m.dispatch(target.call)
	}
	return nil
}
