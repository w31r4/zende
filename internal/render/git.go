package render

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

type gitMsg struct {
	generation uint64
	status     protocol.GitStatus
	err        error
}
type gitTickMsg struct{ generation uint64 }

func (m *Shell) syncGit(force bool) tea.Cmd {
	path := m.snapshot.Agent.Cwd
	if path == "" || m.ctx.Err() != nil {
		return nil
	}
	if path == m.gitPath {
		if m.gitWaiting || !force {
			return nil
		}
	} else {
		m.git = protocol.GitStatus{}
		m.issues.resolve("git")
	}
	m.gitPath, m.gitWaiting = path, true
	m.gitGeneration++
	generation, ctx, client, agent := m.gitGeneration, m.ctx, m.client, m.agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		msg := gitMsg{generation: generation}
		msg.err = queryUI(ctx, client, "workspace.git", protocol.GitQuery{AgentID: agent, Path: path}, &msg.status)
		return msg
	}
}

func gitTick(ctx context.Context, generation uint64) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return gitTickMsg{generation}
		}
	}
}

func (m Shell) gitLabel() string {
	g := m.git
	if g.State != "ready" || g.Path != m.snapshot.Agent.Cwd {
		return ""
	}
	name := g.Branch
	if g.Detached {
		name = g.Commit[:min(8, len(g.Commit))]
	}
	if g.Staged+g.Unstaged+g.Untracked+g.Conflicts > 0 {
		name += "*"
	}
	return singleLine(name)
}

func (m Shell) gitLines() []string {
	g := m.git
	if m.gitWaiting && g.State == "" {
		return []string{"Git", "..."}
	}
	if g.State == "" {
		return []string{"Git"}
	}
	if g.State == "none" {
		return []string{singleLine(g.Path), "Not a Git repository"}
	}
	lines := []string{singleLine(g.Path), m.gitLabel()}
	if g.Unborn {
		lines = append(lines, "No commits")
	}
	if g.Upstream != "" {
		ahead, behind := "?", "?"
		if g.Ahead != nil {
			ahead = fmt.Sprint(*g.Ahead)
		}
		if g.Behind != nil {
			behind = fmt.Sprint(*g.Behind)
		}
		lines = append(lines, singleLine(g.Upstream)+m.theme.separator()+"+"+ahead+" -"+behind)
	}
	var changes []string
	for _, p := range []struct {
		label string
		count int
	}{{"staged", g.Staged}, {"modified", g.Unstaged}, {"untracked", g.Untracked}, {"conflicts", g.Conflicts}} {
		if p.count > 0 {
			changes = append(changes, fmt.Sprintf("%d %s", p.count, p.label))
		}
	}
	if len(changes) > 0 {
		lines = append(lines, strings.Join(changes, m.theme.separator()))
	}
	return lines
}
