package render

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (m Shell) statusLine() string {
	line, _ := m.statusProjection()
	return line
}

func (m Shell) statusProjection() (string, []hitTarget) {
	sep := m.theme.separator()
	type part struct {
		text string
		call invocation
	}
	var right []part
	if count := len(m.questionOptions()); count > 0 {
		right = append(right, part{fmt.Sprintf("? %d", count), invocation{name: "questions"}})
	}
	if work := m.activityLabel(); work != "" {
		right = append(right, part{work, invocation{name: "background"}})
	}
	if len(m.issues.active) > 0 {
		right = append(right, part{fmt.Sprintf("! %d", len(m.issues.active)), invocation{name: "issues"}})
	}
	if m.loading {
		right = append(right, part{text: "connecting"})
	} else if !m.connected {
		right = append(right, part{"offline", invocation{name: "refresh"}})
	} else if m.cursor < m.snapshot.Cursor {
		right = append(right, part{text: "replaying"})
	}
	texts := make([]string, len(right))
	for i, p := range right {
		texts[i] = p.text
	}
	suffix := strings.Join(texts, sep)
	available := max(0, m.width-ansi.StringWidth(suffix))
	if suffix != "" {
		available = max(0, available-1)
	}
	model := ""
	id := m.snapshot.Agent.Model
	if id == "" {
		id = m.transcript.ResolvedModel
	}
	if id != "" {
		parts := strings.Split(id, "/")
		model = m.theme.modelMark() + " " + ansi.Truncate(safeText(parts[len(parts)-1]), min(16, available/3), "...") + sep
	}
	cwd := safeText(m.snapshot.Agent.Cwd)
	room := max(0, available-ansi.StringWidth(model))
	if ansi.StringWidth(cwd) > room {
		if room < 3 {
			cwd = ansi.Truncate(cwd, room, "")
		} else {
			cwd = ansi.TruncateLeft(cwd, ansi.StringWidth(cwd)-room+3, "...")
		}
	}
	left := ansi.Truncate(model+cwd, available, "")
	if suffix == "" {
		return left, nil
	}
	padding := max(1, m.width-ansi.StringWidth(left)-ansi.StringWidth(suffix))
	x := ansi.StringWidth(left) + padding
	var targets []hitTarget
	for _, p := range right {
		w := ansi.StringWidth(p.text)
		if p.call.name != "" {
			targets = append(targets, hitTarget{x: x, y: m.height - 1, width: w, call: p.call, candidate: -1})
		}
		x += w + ansi.StringWidth(sep)
	}
	return left + strings.Repeat(" ", padding) + suffix, targets
}
