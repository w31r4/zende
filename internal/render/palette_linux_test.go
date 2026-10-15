package render

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

type paletteTerminalClient struct{ mutations atomic.Int32 }

func (c *paletteTerminalClient) Retry(_ context.Context, req protocol.Request, target any, _ int, _ time.Duration) error {
	switch req.Method {
	case "agent.inspect":
		*target.(*protocol.Snapshot) = protocol.Snapshot{Agent: protocol.Agent{ID: "agent", Cwd: "/project"}}
	case "agent.activity":
		*target.(*protocol.Activity) = protocol.Activity{AgentID: "agent"}
	case "workspace.git":
		*target.(*protocol.GitStatus) = protocol.GitStatus{Path: "/project", State: "none"}
	default:
		c.mutations.Add(1)
		return fmt.Errorf("unexpected method %s", req.Method)
	}
	return nil
}
func (*paletteTerminalClient) Watch(ctx context.Context, _ protocol.Cursor, _ func(protocol.Event) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type paletteProbe struct{ reply chan paletteState }
type paletteState struct {
	text, held         string
	palette, help      bool
	inputY, candidateY int
}

func TestRealTerminalCommandPalettePreservesMultilineDraft(t *testing.T) {
	master, slave, capture := testTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &paletteTerminalClient{}
	var program *tea.Program
	m := New(ctx, client, "agent", func(msg tea.Msg) { program.Send(msg) })
	program = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(slave), tea.WithOutput(slave), tea.WithWindowSize(80, 24), tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
		if probe, ok := msg.(paletteProbe); ok {
			current := model.(Shell)
			f := current.layout()
			state := paletteState{text: current.input.Value(), palette: current.paletteActive(), help: current.help, inputY: len(f.body), candidateY: -1}
			if current.commandDraft != nil {
				state.held = current.commandDraft.Value()
			}
			if len(f.candidates) > 0 {
				state.candidateY = len(f.body) + 2
			}
			probe.reply <- state
			return nil
		}
		return msg
	}))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	t.Cleanup(func() { program.Kill(); program.Wait() })
	waitTerminal(t, capture, "\x1b[?1049h")
	waitTerminal(t, capture, "\x1b[?1002h")
	waitState := func(check func(paletteState) bool) paletteState {
		t.Helper()
		var state paletteState
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			reply := make(chan paletteState, 1)
			program.Send(paletteProbe{reply})
			select {
			case state = <-reply:
				if check(state) {
					return state
				}
			case <-time.After(time.Second):
				t.Fatal("UI did not answer probe")
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("UI state did not arrive", state)
		return state
	}
	write := func(text string) {
		t.Helper()
		if _, err := master.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	const draft = "中文 é 🙂\nsecond line "
	write("\x1b[200~" + draft + "\x1b[201~/he")
	state := waitState(func(s paletteState) bool { return s.palette && s.text == draft+"/he" })
	if state.candidateY != state.inputY+2 {
		t.Fatal("candidate not below both input rows", state)
	}
	write("\t")
	waitState(func(s paletteState) bool { return !s.palette && s.text == "/help" && s.held == draft && !s.help })
	write("\r")
	waitState(func(s paletteState) bool { return s.help && s.text == draft && s.held == "" })
	write("/back\r")
	waitState(func(s paletteState) bool { return !s.help && s.text == draft })
	// A bracketed paste containing a command stays text until explicit Enter.
	write("\x1b[200~/stop\x1b[201~")
	waitState(func(s paletteState) bool { return !s.palette && s.text == draft+"/stop" })
	write("\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI did not detach")
	}
	waitTerminal(t, capture, "\x1b[?1049l")
	waitTerminal(t, capture, "\x1b[?1002l")
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(before, after) || client.mutations.Load() != 0 {
		t.Fatal("UI changed terminal modes or submitted text", err, client.mutations.Load())
	}
}
