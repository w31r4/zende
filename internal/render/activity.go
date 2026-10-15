package render

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

type activityMsg struct {
	Activity   protocol.Activity
	Err        error
	Generation uint64
}
type activityTickMsg struct{ Generation uint64 }

func activityCmd(ctx context.Context, client Client, agent string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		req, _ := protocol.NewRequest("agent.activity", "", protocol.Target{AgentID: agent})
		var a protocol.Activity
		err := client.Retry(ctx, req, &a, 1, 0)
		if err == nil && (a.AgentID != agent || len(a.Items) > protocol.MaxActivityItems) {
			err = protocol.E("invalid_state", "activity identity or limit changed")
		}
		return activityMsg{a, err, generation}
	}
}
func activityTick(ctx context.Context, generation uint64) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return activityTickMsg{generation}
		}
	}
}
func (m *Shell) startActivity() tea.Cmd {
	if m.activityWaiting || m.ctx.Err() != nil {
		return nil
	}
	m.activityWaiting = true
	m.activityGeneration++
	return activityCmd(m.ctx, m.client, m.agent, m.activityGeneration)
}
func (m Shell) activityLabel() string {
	if len(m.activity.Items) == 0 && !m.activity.Truncated && len(m.activity.Unavailable) == 0 && !m.activityStale {
		return ""
	}
	count := fmt.Sprint(len(m.activity.Items))
	if m.activity.Truncated {
		count += "+"
	}
	if m.activityStale || len(m.activity.Unavailable) > 0 {
		if count == "0" {
			count = "?"
		} else {
			count += "?"
		}
	}
	return m.theme.workMark() + " " + count
}
func (m Shell) activityLines(width, rows int) []string {
	lines := []string{"Background"}
	if m.activityWaiting && len(m.activity.Items) == 0 {
		lines = append(lines, "loading")
	}
	if m.activityStale || len(m.activity.Unavailable) > 0 {
		lines = append(lines, "! partial observation")
	}
	if m.activity.Truncated {
		lines = append(lines, "! first 1000 items; inspect owners for full state")
	}
	available := max(0, rows-len(lines))
	start := min(m.scroll, max(0, len(m.activity.Items)-available))
	for _, item := range m.activity.Items[start:min(len(m.activity.Items), start+available)] {
		kind, state := safeText(item.Kind), safeText(item.State)
		sep := m.theme.separator()
		id := ansi.Truncate(safeText(item.ID), max(0, width-ansi.StringWidth(kind+" "+sep+state)), "...")
		lines = append(lines, ansi.Truncate(kind+" "+id+sep+state, width, "..."))
	}
	if len(m.activity.Items) == 0 && !m.activityWaiting && len(lines) == 1 {
		lines = append(lines, "No background work")
	}
	return lines
}
