package render

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"
)

// Run only attaches a client. Bubble Tea owns terminal cleanup; the context
// here owns read subscriptions and RPC waits, never the background core.
func Run(ctx context.Context, client Client, agent string, in io.Reader, out io.Writer, options ...tea.ProgramOption) error {
	return RunWithAppearance(ctx, client, agent, in, out, Appearance{}, options...)
}

type Appearance struct{ ASCII bool }

func RunWithAppearance(ctx context.Context, client Client, agent string, in io.Reader, out io.Writer, appearance Appearance, options ...tea.ProgramOption) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var program *tea.Program
	model := New(ctx, client, agent, func(msg tea.Msg) { program.Send(msg) })
	model.theme.ascii = appearance.ASCII
	base := []tea.ProgramOption{tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)}
	program = tea.NewProgram(model, append(base, options...)...)
	_, err := program.Run()
	return err
}
