package render

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type issue struct {
	source, kind, text string
	count              int
}
type issues struct {
	active       []issue
	seen         []issue
	open, manual bool
}

func (b *issues) raise(source string, err error) {
	if err == nil {
		b.resolve(source)
		return
	}
	item := issue{source: source, kind: "request_failed", text: "request failed", count: 1}
	var known *protocol.Error
	if errors.As(err, &known) {
		item.kind, item.text = known.Data.Kind, known.Error()
	}
	item.text = ansi.Truncate(safeText(item.text), 512, "...")
	fresh := true
	b.seen = append([]issue(nil), b.seen...)
	for i, old := range b.seen {
		if old.source == source && old.kind == item.kind {
			item.count = min(old.count+1, 999999)
			b.seen[i] = item
			fresh = false
			break
		}
	}
	if fresh {
		b.seen = append(b.seen, item)
		if len(b.seen) > 64 {
			b.seen = b.seen[1:]
		}
	}
	manual := b.manual
	b.resolve(source)
	b.active = append(b.active, item)
	b.manual, b.open = manual, fresh || manual
}
func (b *issues) resolve(source string) {
	active := make([]issue, 0, len(b.active))
	for _, item := range b.active {
		if item.source != source {
			active = append(active, item)
		}
	}
	b.active = active
	if len(active) == 0 {
		b.open, b.manual = false, false
	}
}
func (b issues) lines(width, rows int) []string {
	if !b.open || rows <= 0 {
		return nil
	}
	items := append([]issue(nil), b.active...)
	sort.Slice(items, func(i, j int) bool { return items[i].source < items[j].source })
	var lines []string
	for _, item := range items {
		text := "! " + item.text
		if item.count > 1 {
			text += fmt.Sprintf(" (x%d)", item.count)
		}
		lines = append(lines, strings.Split(ansi.Hardwrap(text, width, true), "\n")...)
	}
	if len(lines) > rows {
		lines = lines[:rows]
		lines[rows-1] = ansi.Truncate(lines[rows-1], max(0, width-3), "") + "..."
	}
	return lines
}
func (m *Shell) report(err error)           { m.err = err; m.issues.raise("request", err) }
func (m *Shell) reportConnection(err error) { m.connectionErr = err; m.issues.raise("connection", err) }
