package process

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"zende.local/harness/internal/protocol"
)

type TerminalController interface {
	Inspect(context.Context, string) (State, error)
	Resize(context.Context, string, ResizeParams) (ControlResult, error)
}

// Attachment is a short-lived tmux client, not a new command in the managed
// PTY. Exiting/cancelling it never signals the managed command or its server.
// SetStdin/Stdout/Stderr also satisfy Bubble Tea's terminal handoff contract
// without importing the UI framework into the process runtime.
type Attachment struct {
	Context          context.Context
	Controller       TerminalController
	ProcessID        string
	in               io.Reader
	out, diagnostics io.Writer
}

func (a *Attachment) SetStdin(r io.Reader)  { a.in = r }
func (a *Attachment) SetStdout(w io.Writer) { a.out = w }
func (a *Attachment) SetStderr(w io.Writer) { a.diagnostics = w }

func (a *Attachment) Run() error {
	input, ok := a.in.(term.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return protocol.E("invalid_params", "terminal attachment requires a TTY input")
	}
	output, ok := a.out.(term.File)
	if !ok || !term.IsTerminal(output.Fd()) {
		return protocol.E("invalid_params", "terminal attachment requires a TTY output")
	}
	ctx, cancel := context.WithCancel(a.Context)
	defer cancel()
	queryCtx, queryCancel := context.WithTimeout(ctx, 5*time.Second)
	state, err := a.Controller.Inspect(queryCtx, a.ProcessID)
	queryCancel()
	if err != nil {
		return err
	}
	if !state.PTY {
		return protocol.E("unsupported", "terminal attachment does not apply to pipe processes")
	}
	if state.ID != a.ProcessID || state.Status != "running" || state.Terminal == nil {
		return protocol.E("process_unavailable", "original terminal is not running")
	}
	binary, err := exec.LookPath("tmux")
	if err != nil {
		return protocol.E("dependency_missing", "terminal attachment requires tmux on PATH")
	}
	pane, err := inspectPane(binary, *state.Terminal)
	if err != nil || pane.Dead {
		return protocol.E("process_unavailable", "original terminal identity could not be verified")
	}
	resize := func() error {
		cols, rows, err := term.GetSize(output.Fd())
		if err != nil {
			return protocol.E("invalid_state", "local terminal size could not be read")
		}
		callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
		defer callCancel()
		_, err = a.Controller.Resize(callCtx, protocol.NewID("attach-size"), ResizeParams{ID: a.ProcessID, Cols: cols, Rows: rows})
		return err
	}
	if err := resize(); err != nil {
		return err
	}
	// Fixed runner-managed window sizes remain explicit. ignore-size prevents
	// another tmux client from implicitly resizing unrelated sessions/windows.
	cmd := exec.CommandContext(ctx, binary, "-N", "-S", state.Terminal.Socket, "-f", "/dev/null", "attach-session", "-f", "ignore-size", "-t", "="+state.Terminal.Session)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, a.out, a.diagnostics
	cmd.WaitDelay = time.Second
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TMUX=") && !strings.HasPrefix(value, "TMUX_PANE=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	sizes := make(chan os.Signal, 1)
	signal.Notify(sizes, syscall.SIGWINCH)
	defer signal.Stop(sizes)
	finished := make(chan struct{})
	resizeDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-finished:
				resizeDone <- nil
				return
			case <-ctx.Done():
				resizeDone <- nil
				return
			case <-sizes:
				if err := resize(); err != nil {
					resizeDone <- err
					cancel()
					return
				}
			}
		}
	}()
	runErr := cmd.Run()
	close(finished)
	// A failed/uncertain resize stops only this observer, leaving the original
	// control record inspectable; a fresh operation cannot bypass that gate.
	resizeErr := <-resizeDone
	if resizeErr != nil {
		return resizeErr
	}
	if a.Context.Err() != nil {
		return a.Context.Err()
	}
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return protocol.E("terminal_attach_failed", "tmux client exited unsuccessfully; original process remains inspectable")
		}
		return protocol.E("terminal_attach_failed", "tmux client could not start")
	}
	return nil
}
