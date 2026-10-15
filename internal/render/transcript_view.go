package render

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Lines formats only the requested window, newest first internally. Terminal
// escapes are always made literal before wrapping. Folding is a read-only view.
func (p *Transcript) Lines(width, rows, scroll int, fold bool) []string {
	return p.lines(width, rows, scroll, fold, " · ")
}
func (p *Transcript) lines(width, rows, scroll int, fold bool, separator string) []string {
	projected := p.viewRows(width, rows, scroll, fold, nil, separator)
	lines := make([]string, len(projected))
	for i, row := range projected {
		lines[i] = row.text
	}
	return lines
}

type transcriptRow struct {
	text, commentaryKey string
}

func (p *Transcript) viewRows(width, rows, scroll int, fold bool, overrides map[string]bool, separator string) []transcriptRow {
	var reverse []transcriptRow
	for i := len(p.Entries) - 1; i >= 0 && len(reverse) < rows+scroll; i-- {
		entry := p.Entries[i]
		if entry.Hidden {
			continue
		}
		prefix, text := "", entry.Text
		folded := fold
		if value, ok := overrides[entry.Key]; ok {
			folded = value
		}
		key := ""
		switch entry.Kind {
		case "mailbox":
			sender := "user"
			if entry.Mail.Source != nil {
				sender = entry.Mail.Source.Caller.AgentID
			}
			if entry.Mail.Result != nil {
				sender = entry.Mail.Result.AgentID
			}
			prefix = "@ " + sender + separator + entry.Mail.Delivery + separator + entry.State + "\n"
		case "question":
			prefix = "? "
			if entry.State == "cancelled" {
				prefix += "[cancelled] "
			}
		case "user":
			prefix = "> "
			if len(entry.Attachments) > 0 {
				if text != "" {
					text += "\n"
				}
				text += fmt.Sprintf("[img %d]", len(entry.Attachments))
			}
			if len(entry.Skills) > 0 {
				if text != "" {
					text += "\n"
				}
				text += fmt.Sprintf("[skill %d]", len(entry.Skills))
			}
			if text == "" {
				continue // a mailbox Task or status-only event is not user text
			}
		case "assistant":
			switch entry.Category {
			case "commentary":
				prefix = "~ "
				key = entry.Key
				if folded {
					text = "..."
				}
			case "final":
				prefix = "= "
			default:
				prefix = "? unclassified "
			}
			if entry.State != "confirmed" {
				prefix += "[" + entry.State + "] "
			}
		case "tool":
			prefix = "$ " + entry.Name + separator + entry.State
			if text != "" {
				prefix += "\n"
			}
		case "notice":
			prefix = "! "
		}
		if entry.Truncated && !(folded && entry.Category == "commentary") {
			text = "[earlier text omitted; retained in history]\n" + text
		}
		lines := strings.Split(ansi.Hardwrap(safeText(prefix+text), max(1, width), true), "\n")
		for j := len(lines) - 1; j >= 0 && len(reverse) < rows+scroll; j-- {
			reverse = append(reverse, transcriptRow{lines[j], key})
		}
	}
	if p.DroppedThrough > 0 && len(reverse) < rows+scroll {
		reverse = append(reverse, transcriptRow{text: ansi.Truncate(fmt.Sprintf("! display window; earlier history retained through seq %d", p.DroppedThrough), width, "...")})
	}
	// Scrolling past the oldest retained row stops at that row, not a blank page.
	scroll = min(scroll, max(0, len(reverse)-rows))
	end := min(len(reverse), scroll+rows)
	lines := make([]transcriptRow, 0, end-scroll)
	for i := end - 1; i >= scroll; i-- {
		lines = append(lines, reverse[i])
	}
	return lines
}
