package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
)

type processPanel struct {
	kind, id, stream            string
	generation                  uint64
	page                        process.ListPage
	selection                   int
	state                       process.State
	control, waiting, truncated bool
	offset                      int64
	data                        []byte
	gap                         *process.Gap
	err                         error
}
type processesMsg struct {
	Generation uint64
	Page       process.ListPage
	Err        error
}
type processLogMsg struct {
	Generation uint64
	Offset     int64
	State      process.State
	Output     process.Output
	Err        error
}
type processPollMsg struct{ Generation uint64 }
type resizedMsg struct {
	Request protocol.Request
	Result  process.ControlResult
	Err     error
}
type attachmentFinishedMsg struct{ Err error }

type terminalClient struct{ client Client }

func (c terminalClient) Inspect(ctx context.Context, id string) (process.State, error) {
	req, _ := protocol.NewRequest("process.inspect", "", process.Target{ID: id})
	var result process.State
	err := c.client.Retry(ctx, req, &result, 3, 100*time.Millisecond)
	return result, err
}
func (c terminalClient) Resize(ctx context.Context, id string, p process.ResizeParams) (process.ControlResult, error) {
	req, err := protocol.NewRequest("process.resize", id, p)
	if err != nil {
		return process.ControlResult{}, err
	}
	var result process.ControlResult
	err = c.client.Retry(ctx, req, &result, 3, 100*time.Millisecond)
	return result, err
}

func processListCmd(ctx context.Context, client Client, agent, after string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, _ := protocol.NewRequest("process.list", "", process.ListParams{AgentID: agent, After: after, Limit: 20})
		var page process.ListPage
		err := client.Retry(ctx, req, &page, 3, 100*time.Millisecond)
		return processesMsg{generation, page, err}
	}
}
func processLogCmd(ctx context.Context, client Client, id, stream string, offset int64, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		state, err := (terminalClient{client}).Inspect(ctx, id)
		msg := processLogMsg{Generation: generation, Offset: offset, State: state, Err: err}
		if err != nil {
			return msg
		}
		if state.ID != id {
			msg.Err = protocol.E("invalid_state", "process identity did not match")
			return msg
		}
		if stream == "" {
			stream = "stdout"
			if state.PTY {
				stream = "pty"
			}
		}
		req, _ := protocol.NewRequest("process.output", "", process.ReadParams{ID: id, Stream: stream, Offset: offset, Limit: 32768})
		msg.Err = client.Retry(ctx, req, &msg.Output, 3, 100*time.Millisecond)
		if msg.Err == nil && (msg.Output.ProcessID != id || msg.Output.Stream != stream || msg.Output.Next < offset || msg.Output.Next > msg.Output.Total || msg.Output.Next-offset < int64(len(msg.Output.Data))) {
			msg.Err = protocol.E("cursor_gap", "process output identity or cursor changed")
		}
		if msg.Err == nil {
			end := offset + int64(len(msg.Output.Data))
			gap := msg.Output.Gap
			if (gap == nil && end != msg.Output.Next) || (gap != nil && (gap.From != end || gap.To != msg.Output.Next || gap.To <= gap.From)) {
				msg.Err = protocol.E("cursor_gap", "process output omitted an unexplained range")
			}
		}
		return msg
	}
}
func pollProcessCmd(ctx context.Context, generation uint64, delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return processPollMsg{generation}
		}
	}
}
func resizeCmd(ctx context.Context, client Client, req protocol.Request) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var result process.ControlResult
		err := client.Retry(ctx, req, &result, 3, 100*time.Millisecond)
		return resizedMsg{req, result, err}
	}
}

func (m *Shell) openProcesses(after string) tea.Cmd {
	m.closePanel()
	m.panel = &processPanel{kind: "list", generation: m.processGeneration, waiting: true}
	return processListCmd(m.ctx, m.client, m.agent, after, m.processGeneration)
}
func (m *Shell) openLog(id, stream string, control bool) tea.Cmd {
	m.closePanel()
	m.panel = &processPanel{kind: "log", id: id, stream: stream, generation: m.processGeneration, waiting: true, control: control}
	m.scroll = 0
	return processLogCmd(m.ctx, m.client, id, stream, 0, m.processGeneration)
}
func (m *Shell) resizeSelected() tea.Cmd {
	p := m.panel
	if m.copyFlow != nil || m.attaching || m.resizePending != nil || p == nil || p.kind != "log" || !p.control || !p.state.PTY || p.state.Status != "running" || p.state.Terminal == nil {
		return nil
	}
	cols, rows := min(1000, max(2, m.width)), min(1000, max(2, m.height))
	if p.state.Terminal.Cols == cols && p.state.Terminal.Rows == rows {
		return nil
	}
	req, err := protocol.NewRequest("process.resize", protocol.NewID("ui-size"), process.ResizeParams{ID: p.id, Cols: cols, Rows: rows})
	if err != nil {
		m.report(err)
		return nil
	}
	m.resizePending = &req
	return resizeCmd(m.ctx, m.client, req)
}

// Process commands are explicit UI requests. Ordinary chat submission never
// writes stdin; raw interaction is confined to the chosen terminal handoff.
func (m *Shell) runProcessCommand(name string, args []string) (tea.Cmd, error) {
	switch name {
	case "terminal", "logs":
		if len(args) == 0 {
			cmd := m.openProcesses("")
			m.panel.control = name == "terminal"
			return cmd, nil
		}
		stream := ""
		if len(args) == 2 {
			stream = args[1]
		}
		if stream != "" && stream != "stdout" && stream != "stderr" && stream != "pty" {
			return nil, protocol.E("invalid_params", "unknown output stream")
		}
		return m.openLog(args[0], stream, name == "terminal"), nil
	case "next":
		if m.panel == nil || m.panel.kind != "list" || m.panel.page.Next == "" {
			return nil, protocol.E("not_found", "no next process page")
		}
		control, after := m.panel.control, m.panel.page.Next
		cmd := m.openProcesses(after)
		m.panel.control = control
		return cmd, nil
	case "resize":
		if m.resizePending != nil {
			if len(args) != 0 {
				return nil, protocol.E("request_pending", "use /resize without arguments to resolve the pending resize first")
			}
			return resizeCmd(m.ctx, m.client, *m.resizePending), nil
		}
		if m.copyFlow != nil {
			return nil, protocol.E("copy_pending", "source terminal control is frozen during session copying")
		}
		if m.panel == nil || !m.panel.state.PTY {
			return nil, protocol.E("unsupported", "select a PTY with /terminal PROCESS")
		}
		if len(args) != 2 {
			return nil, protocol.E("invalid_params", "use /resize COLS ROWS")
		}
		cols, e1 := strconv.Atoi(args[0])
		rows, e2 := strconv.Atoi(args[1])
		if e1 != nil || e2 != nil || cols < 2 || rows < 2 || cols > 1000 || rows > 1000 {
			return nil, protocol.E("invalid_params", "terminal size must be 2–1000 per axis")
		}
		req, _ := protocol.NewRequest("process.resize", protocol.NewID("ui-size"), process.ResizeParams{ID: m.panel.id, Cols: cols, Rows: rows})
		m.panel.control = false // Explicit fixed size, until /terminal selects control again.
		m.resizePending = &req
		return resizeCmd(m.ctx, m.client, req), nil
	case "attach":
		if m.copyFlow != nil {
			return nil, protocol.E("copy_pending", "source terminal control is frozen during session copying")
		}
		id := ""
		if m.panel != nil {
			id = m.panel.id
		}
		if len(args) == 1 {
			id = args[0]
		}
		if id == "" {
			return nil, protocol.E("invalid_params", "use /attach PROCESS; Ctrl+B then d returns")
		}
		if m.resizePending != nil {
			return nil, protocol.E("request_pending", "resize is unresolved; /resize retries its original ID")
		}
		m.attaching = true
		if m.watchCancel != nil {
			m.watchCancel()
		}
		m.generation++
		m.watching, m.connected, m.retryPending = false, false, false
		m.processGeneration++
		if m.panel != nil {
			m.panel.generation = m.processGeneration
			m.panel.waiting = false
		}
		attachment := &process.Attachment{Context: m.ctx, Controller: terminalClient{m.client}, ProcessID: id}
		return tea.Exec(attachment, func(err error) tea.Msg { return attachmentFinishedMsg{err} }), nil
	}
	return nil, protocol.E("invalid_params", "unknown process command")
}

func (m *Shell) updateProcess(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case processesMsg:
		if m.panel == nil || m.panel.generation != msg.Generation {
			return true, nil
		}
		m.panel.waiting, m.panel.err, m.panel.page = false, msg.Err, msg.Page
		m.issues.raise("process", msg.Err)
		return true, nil
	case processPollMsg:
		if m.attaching || m.panel == nil || m.panel.generation != msg.Generation || m.panel.waiting {
			return true, nil
		}
		p := m.panel
		p.waiting = true
		return true, processLogCmd(m.ctx, m.client, p.id, p.stream, p.offset, p.generation)
	case processLogMsg:
		p := m.panel
		if p == nil || p.generation != msg.Generation {
			return true, nil
		}
		if msg.Offset != p.offset {
			return true, nil
		}
		p.waiting, p.err = false, msg.Err
		m.issues.raise("process", msg.Err)
		if msg.Err != nil {
			return true, nil
		}
		p.state, p.stream, p.offset = msg.State, msg.Output.Stream, msg.Output.Next
		p.data = append(p.data, msg.Output.Data...)
		if len(p.data) > 64<<10 {
			p.data = append([]byte(nil), p.data[len(p.data)-(64<<10):]...)
			p.truncated = true
		}
		if msg.Output.Gap != nil {
			gap := *msg.Output.Gap
			if p.gap != nil && p.gap.To == gap.From {
				gap.From = p.gap.From
			}
			p.gap = &gap
		}
		var poll tea.Cmd
		if msg.Output.Next < msg.Output.Total {
			poll = pollProcessCmd(m.ctx, p.generation, 0)
		} else if msg.Output.Status == "running" || msg.Output.Status == "starting" || msg.Output.Status == "unknown" {
			poll = pollProcessCmd(m.ctx, p.generation, 500*time.Millisecond)
		}
		return true, tea.Batch(poll, m.resizeSelected())
	case resizedMsg:
		if m.resizePending == nil || m.resizePending.OperationID != msg.Request.OperationID {
			return true, nil
		}
		var target process.ResizeParams
		if msg.Err == nil && (json.Unmarshal(m.resizePending.Params, &target) != nil || msg.Result.State.ID != target.ID || msg.Result.State.Terminal == nil || msg.Result.State.Terminal.Cols != target.Cols || msg.Result.State.Terminal.Rows != target.Rows) {
			msg.Err = protocol.E("transport_unknown", "resize result did not confirm the original target and size")
		}
		if msg.Err != nil {
			m.report(msg.Err)
			var known *protocol.Error
			if errors.As(msg.Err, &known) && known.Data.Kind != "transport_unknown" && known.Data.Kind != "operation_unknown" && known.Data.Kind != "unavailable" {
				m.resizePending = nil
			}
			return true, nil
		}
		m.resizePending = nil
		m.report(nil)
		if m.panel != nil && m.panel.id == msg.Result.State.ID {
			m.panel.state = msg.Result.State
		}
		return true, m.resizeSelected()
	case attachmentFinishedMsg:
		m.attaching = false
		m.report(msg.Err)
		cmd := m.startWatch()
		m.refreshing = true
		var poll tea.Cmd
		if m.panel != nil && m.panel.kind == "log" {
			poll = pollProcessCmd(m.ctx, m.panel.generation, 0)
		}
		return true, tea.Batch(cmd, snapshotCmd(m.ctx, m.client, m.agent), poll)
	}
	return false, nil
}

func (m *Shell) panelKey(key string) (bool, tea.Cmd) {
	if m.panel == nil {
		return false, nil
	}
	if key == "esc" {
		return true, m.dispatch(invocation{name: "back"})
	}
	if m.panel.kind != "list" || m.input.Value() != "" {
		return false, nil
	}
	switch key {
	case "up", "k":
		m.panel.selection = max(0, m.panel.selection-1)
		return true, nil
	case "down", "j":
		m.panel.selection = min(max(0, len(m.panel.page.Processes)-1), m.panel.selection+1)
		return true, nil
	case "n":
		if m.panel.page.Next != "" {
			return true, m.dispatch(invocation{name: "next"})
		}
		return true, nil
	case "enter":
		if len(m.panel.page.Processes) > 0 {
			id := m.panel.page.Processes[m.panel.selection].ID
			name := "logs"
			if m.panel.control {
				name = "terminal"
			}
			return true, m.dispatch(invocation{name: name, args: []string{id}})
		}
		return true, nil
	}
	return false, nil
}

func (p *processPanel) lines(width, rows, scroll int) []string {
	return p.renderLines(width, rows, scroll, " · ", true)
}
func (p *processPanel) renderLines(width, rows, scroll int, sep string, showError bool) []string {
	if rows <= 0 || width <= 0 {
		return nil
	}
	var lines []string
	var footer []string
	if p.kind == "list" && p.page.Next != "" {
		footer = append(footer, "n next page")
	}
	if p.kind == "log" {
		if p.truncated {
			footer = append(footer, "! earlier bytes retained in process output")
		}
		if p.gap != nil {
			footer = append(footer, fmt.Sprintf("! missing bytes %d..%d", p.gap.From, p.gap.To))
		}
	}
	if p.err != nil && showError {
		var known *protocol.Error
		text := "process query failed"
		if errors.As(p.err, &known) {
			text = known.Error()
		}
		footer = append(footer, "! "+safeText(text))
	}
	// Keep the target heading and reserve footer space before choosing data rows.
	// At tiny sizes, prioritize errors and gaps over the cache-retention notice.
	footer = footer[max(0, len(footer)-(rows-1)):]
	available := rows - 1 - len(footer)
	if p.kind == "list" {
		lines = append(lines, "Terminals"+sep+"Enter select"+sep+"Esc back")
		start := max(0, p.selection-available+1)
		for i := start; i < len(p.page.Processes) && len(lines) <= available; i++ {
			state := p.page.Processes[i]
			mark := "  "
			if i == p.selection {
				mark = "> "
			}
			kind := "pipe"
			if state.PTY {
				kind = "pty"
			}
			lines = append(lines, ansi.Truncate(safeText(mark+state.ID+sep+kind+sep+state.Status+sep+state.Command), width, "..."))
		}
		if p.waiting && len(lines) <= available {
			lines = append(lines, "loading")
		} else if len(p.page.Processes) == 0 && p.err == nil && len(lines) <= available {
			lines = append(lines, "No processes")
		}
	} else {
		title := p.id + sep + p.stream + sep + p.state.Status
		if p.control && p.state.PTY {
			title += sep + "control"
		}
		lines = append(lines, ansi.Truncate(safeText(title), width, "..."))
		if available > 0 {
			body := strings.Split(ansi.Hardwrap(process.DisplayText(p.data), width, true), "\n")
			end := len(body) - min(max(0, scroll), max(0, len(body)-available))
			lines = append(lines, body[max(0, end-available):end]...)
		}
	}
	lines = append(lines, footer...)
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "...")
	}
	return lines
}
