package cli

import (
	"context"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"strings"
	"time"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/render"
)

func (a *app) tuiCommands(root *cobra.Command) {
	var plain, ascii bool
	command := &cobra.Command{Use: "tui AGENT", Short: "Attach an alt-screen client; leaving it keeps background work running", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		// An explicit/plain or redirected invocation remains a one-shot JSON
		// query. Streaming and submission already have non-interactive commands.
		out, outputFD := cmd.OutOrStdout().(interface{ Fd() uintptr })
		if plain || a.jsonOutput || !outputFD || !term.IsTerminal(out.Fd()) {
			return a.call(cmd, "agent.inspect", false, protocol.Target{AgentID: args[0]})
		}
		in, inputFD := cmd.InOrStdin().(interface{ Fd() uintptr })
		if !inputFD || !term.IsTerminal(in.Fd()) {
			return protocol.E("invalid_params", "TUI requires terminal input; use agent submit --file - for piped text")
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		return render.RunWithAppearance(cmd.Context(), tuiClient{core: protocol.Client{Socket: p.Socket}, runner: protocol.Client{Socket: p.RunnerSocket}}, args[0], cmd.InOrStdin(), cmd.OutOrStdout(), render.Appearance{ASCII: ascii})
	}}
	command.Flags().BoolVar(&plain, "plain", false, "Print a JSON snapshot without taking over the terminal")
	command.Flags().BoolVar(&ascii, "ascii", false, "Use plain-character interface symbols")
	root.AddCommand(command)
}

type tuiClient struct{ core, runner protocol.Client }

func (c tuiClient) Retry(ctx context.Context, req protocol.Request, result any, attempts int, base time.Duration) error {
	client := c.core
	if strings.HasPrefix(req.Method, "process.") {
		client = c.runner
	}
	return client.Retry(ctx, req, result, attempts, base)
}
func (c tuiClient) Watch(ctx context.Context, cursor protocol.Cursor, consume func(protocol.Event) error) error {
	return c.core.Watch(ctx, cursor, consume)
}
func (c tuiClient) WatchReady(ctx context.Context, cursor protocol.Cursor, ready func(), consume func(protocol.Event) error) error {
	return c.core.WatchReady(ctx, cursor, ready, consume)
}
