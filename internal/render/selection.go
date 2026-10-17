package render

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/protocol"
)

// Browsing is local state. Only /select makes a durable, revision-fenced write.
// A directory page and a catalog page never borrow the client's working directory.
type selectionPanel struct {
	kind                      string
	waiting, ready            bool
	index                     int
	base                      protocol.AgentSelection
	connections               protocol.ConnectionPage
	models                    protocol.ModelPage
	directory                 protocol.DirectoryPage
	connection, model, effort string
	version                   string
	copy                      bool
}

type selectionMsg struct {
	generation  uint64
	base        *protocol.AgentSelection
	connections protocol.ConnectionPage
	models      protocol.ModelPage
	directory   protocol.DirectoryPage
	err         error
}

type selectionRow struct {
	text string
	call invocation
}

func queryUI(ctx context.Context, client Client, method string, params, result any) error {
	req, err := protocol.NewRequest(method, "", params)
	if err != nil {
		return err
	}
	return client.Retry(ctx, req, result, 2, 100*time.Millisecond)
}

func (m *Shell) selectionQuery(kind, path, after string, initial bool) tea.Cmd {
	p := *m.selection
	p.kind, p.waiting, p.ready, p.index = kind, true, false, 0
	m.selection = &p
	m.selectionGeneration++
	generation, ctx, client, agent := m.selectionGeneration, m.ctx, m.client, m.agent
	// Capture values, not a pointer later modified by Update.
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		result := selectionMsg{generation: generation}
		if initial {
			base := protocol.AgentSelection{}
			result.err = queryUI(ctx, client, "agent.selection", protocol.Target{AgentID: agent}, &base)
			if result.err != nil {
				return result
			}
			if base.AgentID != agent || !filepath.IsAbs(base.Cwd) {
				result.err = protocol.E("invalid_state", "selection belongs to another Agent or has no absolute cwd")
				return result
			}
			result.base = &base
			p.base = base
		}
		switch kind {
		case "connections":
			result.err = queryUI(ctx, client, "model.connections", protocol.ConnectionQuery{AgentID: agent, After: after, Limit: 40, Version: p.version}, &result.connections)
		case "models":
			result.err = queryUI(ctx, client, "model.list", protocol.ModelQuery{AgentID: agent, Connection: p.connection, After: after, Limit: 40, Version: p.version}, &result.models)
		case "directory":
			if path == "" {
				path = p.base.Cwd
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(p.base.Cwd, path)
			}
			result.err = queryUI(ctx, client, "workspace.directory", protocol.DirectoryQuery{AgentID: agent, Path: path, After: after, Limit: 40}, &result.directory)
		}
		return result
	}
}

func (m *Shell) updateSelection(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case selectionMsg:
		p := m.selection
		if p == nil || msg.generation != m.selectionGeneration || !p.waiting {
			return true, nil
		}
		p.waiting = false
		if msg.err == nil {
			switch p.kind {
			case "connections":
				if msg.connections.Version == "" || p.version != "" && p.version != msg.connections.Version {
					msg.err = protocol.E("catalog_changed", "model catalog changed; reopen /model")
				}
			case "models":
				if msg.models.Version != p.version || msg.models.Connection.ID != p.connection {
					msg.err = protocol.E("catalog_changed", "model catalog changed; reopen /model")
				}
			case "directory":
				if !filepath.IsAbs(msg.directory.Path) || msg.directory.Parent != "" && !filepath.IsAbs(msg.directory.Parent) {
					msg.err = protocol.E("invalid_state", "directory response has no absolute path")
				}
			}
		}
		m.issues.raise("selection", msg.err)
		if msg.err != nil {
			return true, nil
		}
		if msg.base != nil {
			p.base = *msg.base
		}
		p.ready = true
		switch p.kind {
		case "connections":
			p.connections, p.version = msg.connections, msg.connections.Version
		case "models":
			p.models = msg.models
		case "directory":
			p.directory = msg.directory
		}
		return true, nil
	case gitMsg:
		if msg.generation != m.gitGeneration || !m.gitWaiting {
			return true, nil
		}
		m.gitWaiting = false
		if msg.err == nil && (msg.status.Path != m.gitPath || msg.status.State != "ready" && msg.status.State != "none") {
			msg.err = protocol.E("invalid_state", "Git response does not match the working directory")
		}
		m.issues.raise("git", msg.err)
		m.git = protocol.GitStatus{} // Never leave the old branch visible after failure.
		if msg.err == nil {
			m.git = msg.status
		}
		return true, gitTick(m.ctx, m.gitGeneration)
	case gitTickMsg:
		if msg.generation != m.gitGeneration {
			return true, nil
		}
		return true, m.syncGit(true)
	}
	return false, nil
}

func (m *Shell) selectionCommand(name string, args []string) (tea.Cmd, error) {
	p := m.selection
	switch name {
	case "model":
		if len(args) == 0 {
			m.closePanel()
			m.selection = &selectionPanel{}
			return m.selectionQuery("connections", "", "", true), nil
		}
		if p == nil || p.waiting || !p.ready {
			return nil, protocol.E("invalid_params", "open /model and wait for the configured choices")
		}
		if len(args) == 1 && p.kind == "connections" {
			for _, c := range p.connections.Items {
				if c.ID == args[0] {
					if !c.Supported {
						return nil, protocol.E("unsupported", "this protocol adapter is not available")
					}
					p.connection = c.ID
					return m.selectionQuery("models", "", "", false), nil
				}
			}
		}
		if len(args) == 2 && p.kind == "models" && args[0] == p.connection {
			for _, model := range p.models.Items {
				if model.ID == args[1] {
					p.model, p.effort, p.index = model.ID, "", 0
					p.kind = "efforts"
					return nil, nil
				}
			}
		}
		return nil, protocol.E("invalid_params", "select a model from the current catalog page")
	case "effort":
		if p == nil || p.kind != "efforts" || !p.ready {
			return nil, protocol.E("invalid_params", "choose a model with /model first")
		}
		value := ""
		if len(args) > 0 {
			value = args[0]
		}
		valid := value == ""
		for _, e := range p.efforts() {
			valid = valid || value == e
		}
		if !valid {
			return nil, protocol.E("invalid_params", "effort is not supported by this model")
		}
		p.effort, p.kind, p.index = value, "confirm", 0
	case "select":
		if p == nil || !p.ready || p.waiting || p.kind != "directory" && p.kind != "confirm" {
			return nil, protocol.E("invalid_params", "choose a directory or model before confirming")
		}
		if len(args) != 0 && (p.kind != "confirm" || args[0] != "strict" && args[0] != "public") {
			return nil, protocol.E("invalid_params", "use /select strict or /select public when confirming a model")
		}
		if p.copy && p.kind == "directory" {
			p.kind, p.index = "copy-confirm", 2 // Cancel is the default confirmation row.
			return nil, nil
		}
		revision := p.base.Revision
		choice := protocol.SelectAgent{AgentID: m.agent, ExpectedRevision: &revision}
		if p.kind == "directory" {
			cwd := p.directory.Path
			choice.Cwd = &cwd
		} else {
			model, effort := p.connection+"/"+p.model, p.effort
			choice.Model, choice.Effort, choice.CatalogVersion = &model, &effort, p.version
			if len(args) != 0 {
				choice.ContextMode = &args[0]
			}
		}
		cmd, err := m.mutate("agent.select", choice, "")
		if err == nil {
			m.selectionMutation = m.selectionGeneration
		}
		return cmd, err
	case "cd":
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		if p != nil && p.kind == "directory" && p.ready && !p.waiting {
			if path == "" {
				path = p.directory.Path
			} else if !filepath.IsAbs(path) {
				path = filepath.Join(p.directory.Path, path)
			}
			return m.selectionQuery("directory", path, "", false), nil
		}
		m.closePanel()
		m.selection = &selectionPanel{}
		return m.selectionQuery("directory", path, "", true), nil
	case "git":
		m.closePanel()
		m.selection = &selectionPanel{kind: "git"}
		return m.syncGit(true), nil
	}
	return nil, nil
}

func (p *selectionPanel) efforts() []string {
	for _, model := range p.models.Items {
		if model.ID == p.model {
			return model.Efforts
		}
	}
	return nil
}

func (m *Shell) selectionNext() (tea.Cmd, error) {
	p := m.selection
	if p == nil || p.waiting || !p.ready {
		return nil, protocol.E("not_found", "no next page")
	}
	switch {
	case p.kind == "connections" && p.connections.Next != "":
		return m.selectionQuery(p.kind, "", p.connections.Next, false), nil
	case p.kind == "models" && p.models.Next != "":
		return m.selectionQuery(p.kind, "", p.models.Next, false), nil
	case p.kind == "directory" && p.directory.Next != "":
		return m.selectionQuery(p.kind, p.directory.Path, p.directory.Next, false), nil
	}
	return nil, protocol.E("not_found", "no next page")
}

func (m Shell) selectionChoices() (string, []selectionRow) {
	p, sep := m.selection, m.theme.separator()
	title := "Model"
	switch p.kind {
	case "models":
		title = p.connection
	case "efforts", "confirm":
		title = p.connection + "/" + p.model
	case "directory", "copy-confirm":
		title = p.directory.Path
		if title == "" {
			title = "Directory"
		}
		if p.copy {
			title = "Copy" + sep + title
		}
	case "copy-progress":
		return m.copyChoices()
	}
	if p.waiting {
		return title, []selectionRow{{text: "..."}}
	}
	if !p.ready {
		return title, nil
	}
	var rows []selectionRow
	next := ""
	switch p.kind {
	case "connections":
		for _, c := range p.connections.Items {
			label := c.ID
			if c.Label != "" && c.Label != c.ID {
				label = c.Label + sep + c.ID // Same-brand endpoints remain distinct.
			}
			if !c.Supported {
				label += sep + "unavailable"
			}
			rows = append(rows, selectionRow{m.theme.modelMark() + " " + label, invocation{"model", []string{c.ID}}})
		}
		if len(rows) == 0 {
			rows = append(rows, selectionRow{text: "No configured connections"})
		}
		next = p.connections.Next
	case "models":
		for _, model := range p.models.Items {
			label := model.ID
			if model.Label != "" && model.Label != model.ID {
				label = model.Label + sep + model.ID
			}
			rows = append(rows, selectionRow{label, invocation{"model", []string{p.connection, model.ID}}})
		}
		if len(rows) == 0 {
			rows = append(rows, selectionRow{text: "No configured models"})
		}
		next = p.models.Next
	case "efforts":
		rows = append(rows, selectionRow{"default", invocation{name: "effort"}})
		for _, e := range p.efforts() {
			if e != "" {
				rows = append(rows, selectionRow{e, invocation{"effort", []string{e}}})
			}
		}
	case "confirm":
		effort := p.effort
		if effort == "" {
			effort = "default"
		}
		rows = append(rows, selectionRow{effort + sep + "Confirm", invocation{name: "select"}})
	case "directory":
		rows = append(rows, selectionRow{".", invocation{name: "select"}})
		if p.directory.Parent != "" {
			rows = append(rows, selectionRow{"..", invocation{"cd", []string{p.directory.Parent}}})
		}
		for _, entry := range p.directory.Entries {
			label := entry.Name + "/"
			if entry.Symlink {
				label += "@"
			}
			rows = append(rows, selectionRow{label, invocation{"cd", []string{filepath.Join(p.directory.Path, entry.Name)}}})
		}
		if p.directory.Skipped > 0 {
			rows = append(rows, selectionRow{text: fmt.Sprintf("! %d unreadable", p.directory.Skipped)})
		}
		next = p.directory.Next
	case "copy-confirm":
		rows = append(rows, selectionRow{text: "Stop source Agent tree and processes"},
			selectionRow{"Confirm copy", invocation{name: "copy-confirm"}},
			selectionRow{"Cancel", invocation{name: "back"}})
	}
	if next != "" {
		rows = append(rows, selectionRow{"...", invocation{name: "next"}})
	}
	return title, rows
}

func (m Shell) selectionRows(f *frame, rows int) {
	if m.selection.kind == "git" {
		f.body = m.gitLines()
		return
	}
	title, items := m.selectionChoices()
	f.body = append(f.body, singleLine(title))
	available := max(0, rows-1)
	start := max(0, m.selection.index-available+1)
	for i := start; i < len(items) && len(f.body) < rows; i++ {
		mark := "  "
		if i == m.selection.index && items[i].call.name != "" {
			mark = "> "
		}
		text := ansi.Truncate(mark+singleLine(items[i].text), m.width, "...")
		if items[i].call.name != "" {
			f.targets = append(f.targets, hitTarget{x: 0, y: len(f.body), width: ansi.StringWidth(text), call: items[i].call, candidate: -1})
		}
		f.body = append(f.body, text)
	}
}

func (m *Shell) selectionKey(key string) (bool, tea.Cmd) {
	p := m.selection
	if p == nil || m.commandDraft != nil {
		return false, nil
	}
	if key == "esc" {
		return true, m.dispatch(invocation{name: "back"})
	}
	if key == "/" || key == "$" {
		// The picker owns navigation focus, even with a pre-existing draft.
		// Slash/dollar temporarily open the composer; Esc restores the draft.
		draft := m.input
		m.commandDraft = &draft
		m.input = NewComposer()
		m.input.SetWidth(m.width)
		m.input.setTheme(m.theme)
		m.input.SetValue(key)
		m.syncPaletteKey()
		if key == "$" {
			return true, m.loadSkills(false)
		}
		return true, nil
	}
	if key == "left" || key == "h" {
		switch p.kind {
		case "directory":
			if p.ready && p.directory.Parent != "" {
				return true, m.dispatch(invocation{"cd", []string{p.directory.Parent}})
			}
		case "confirm":
			p.kind, p.index = "efforts", 0
		case "copy-confirm":
			p.kind, p.index = "directory", 0
		case "efforts":
			p.kind, p.index = "models", 0
		case "models":
			return true, m.selectionQuery("connections", "", "", false)
		}
		return true, nil
	}
	_, items := m.selectionChoices()
	switch key {
	case "up", "k":
		p.index = max(0, p.index-1)
	case "down", "j":
		p.index = min(max(0, len(items)-1), p.index+1)
	case "pgup":
		p.index = max(0, p.index-max(1, m.height-5))
	case "pgdown":
		p.index = min(max(0, len(items)-1), p.index+max(1, m.height-5))
	case "enter", "right", "l":
		if p.index < len(items) && items[p.index].call.name != "" {
			if key != "enter" && (items[p.index].call.name == "select" || items[p.index].call.name == "copy-confirm") {
				return true, nil // Traversal keys never confirm a durable change.
			}
			return true, m.dispatch(items[p.index].call)
		}
	case "ctrl+c", "ctrl+z":
		return false, nil
	default:
		// Modal navigation never appends stray hjkl / Enter to a held draft.
		return true, nil
	}
	return true, nil
}
