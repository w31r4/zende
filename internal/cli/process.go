package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

func (a *app) serveRunner(cmd *cobra.Command, _ []string) error {
	p, err := a.paths()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := p.EnsureState(); err != nil {
		return err
	}
	proxy := &deferredHandler{}
	server, err := protocol.Listen(p.RunnerSocket, proxy)
	if err != nil {
		return err
	}
	defaults := resource.Defaults(cwd)
	manager, err := process.NewManager(filepath.Join(p.State, "processes"), defaults.Shell, defaults.Limits.Processes, defaults.Limits.OutputBytes)
	if err != nil {
		ctx, cancel := context.WithCancel(cmd.Context())
		cancel()
		server.Serve(ctx)
		return err
	}
	manager.InitializeConfiguration(cmd.Context(), func(ctx context.Context) (process.Configuration, error) {
		req, err := protocol.NewRequest("config.runner", "", struct{}{})
		var config process.Configuration
		if err == nil {
			err = (protocol.Client{Socket: p.Socket}).Call(ctx, req, &config)
		}
		return config, err
	}, func() (process.Configuration, error) {
		_, effective, err := a.environment()
		config := effective.Config
		return process.Configuration{Shell: config.Shell, MaxProcesses: config.Limits.Processes, MaxOutput: config.Limits.OutputBytes}, err
	})
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	finished := make(chan struct{})
	proxy.handler = &serviceControl{handler: process.Service{Manager: manager}, service: "runner", instance: manager.Status().Instance,
		cancel: cancel, prepare: manager.PrepareShutdown, reopen: manager.CancelShutdown, drain: func() { <-finished }}
	go func() {
		defer close(finished)
		manager.FollowConfiguration(ctx)
	}()
	err = server.Serve(ctx)
	cancel()
	<-finished
	return err
}

func (a *app) processCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "process", Short: "Run and inspect persistent commands"}
	var listParams process.ListParams
	list := &cobra.Command{Use: "list", Short: "List observed processes, including completed work", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.call(cmd, "process.list", false, listParams)
	}}
	list.Flags().StringVar(&listParams.AgentID, "agent", "", "Filter by owning Agent")
	list.Flags().StringVar(&listParams.After, "after", "", "Continue after a process ID")
	list.Flags().IntVar(&listParams.Limit, "limit", 20, "Maximum records, 1–100")
	group.AddCommand(list)
	group.AddCommand(&cobra.Command{Use: "attach PROCESS", Short: "Control an existing PTY; Ctrl+B then d returns without stopping it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if a.jsonOutput {
			return protocol.E("invalid_params", "attach is interactive; use process inspect/output for JSON")
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		attachment := &process.Attachment{Context: cmd.Context(), Controller: process.Remote{Client: protocol.Client{Socket: p.RunnerSocket}}, ProcessID: args[0]}
		attachment.SetStdin(cmd.InOrStdin())
		attachment.SetStdout(cmd.OutOrStdout())
		attachment.SetStderr(cmd.ErrOrStderr())
		return attachment.Run()
	}})
	var cwd string
	var pty bool
	var cols, rows int
	start := &cobra.Command{Use: "exec COMMAND", Short: "Start a command and return its process identity", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, e, err := a.environment()
		if err != nil {
			return err
		}
		selected := cwd
		if selected == "" {
			selected = e.Config.Cwd
		}
		selected, err = filepath.Abs(selected)
		if err != nil {
			return err
		}
		id := a.operationID
		if id == "" {
			id = protocol.NewID("process")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		state, err := (process.Remote{Client: protocol.Client{Socket: p.RunnerSocket}}).Start(ctx, process.Spec{ID: id, Cwd: selected, Command: args[0], PTY: pty, Cols: cols, Rows: rows})
		if err != nil {
			return err
		}
		return encode(cmd, state)
	}}
	start.Flags().StringVar(&cwd, "cwd", "", "Working directory")
	start.Flags().BoolVar(&pty, "pty", false, "Run in a persistent tmux terminal")
	start.Flags().IntVar(&cols, "cols", 0, "PTY columns (0 uses 80)")
	start.Flags().IntVar(&rows, "rows", 0, "PTY rows (0 uses 24)")
	group.AddCommand(start)
	group.AddCommand(&cobra.Command{Use: "inspect PROCESS", Short: "Query original process identity and status", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.inspect", false, process.Target{ID: args[0]})
	}})
	var stream string
	var offset int64
	var limit int
	output := &cobra.Command{Use: "output PROCESS", Short: "Read raw output bytes with an explicit cursor", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.output", false, process.ReadParams{ID: args[0], Stream: stream, Offset: offset, Limit: limit})
	}}
	output.Flags().StringVar(&stream, "stream", "stdout", "stdout, stderr or pty")
	output.Flags().Int64Var(&offset, "offset", 0, "Byte cursor")
	output.Flags().IntVar(&limit, "limit", 32768, "Maximum bytes (up to 1 MiB)")
	group.AddCommand(output)
	var waitMS int
	wait := &cobra.Command{Use: "wait PROCESS", Short: "Bounded wait; timeout leaves the process running", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.paths()
		if err != nil {
			return err
		}
		req, _ := protocol.NewRequest("process.wait", "", process.WaitParams{ID: args[0], Milliseconds: waitMS})
		ctx, cancel := context.WithTimeout(cmd.Context(), 35*time.Second)
		defer cancel()
		var result json.RawMessage
		if err := (protocol.Client{Socket: p.RunnerSocket}).Call(ctx, req, &result); err != nil {
			return err
		}
		return encode(cmd, result)
	}}
	wait.Flags().IntVar(&waitMS, "wait-ms", 1000, "Wait duration, 0–30000 ms")
	group.AddCommand(wait)
	var inputFile string
	var closeInput bool
	input := &cobra.Command{Use: "stdin PROCESS [TEXT]", Short: "Send bytes to the original process; optionally close input", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 2 && inputFile != "" {
			return protocol.E("invalid_params", "use TEXT or --file, not both")
		}
		var data []byte
		if len(args) == 2 {
			data = []byte(args[1])
		}
		if inputFile != "" {
			var reader io.Reader = cmd.InOrStdin()
			if inputFile != "-" {
				f, err := os.Open(inputFile)
				if err != nil {
					return protocol.E("invalid_params", "input file could not be opened")
				}
				defer f.Close()
				reader = f
			}
			var err error
			data, err = io.ReadAll(io.LimitReader(reader, process.MaxInputBytes+1))
			if err != nil {
				return protocol.E("invalid_params", "input bytes could not be read")
			}
		}
		if len(data) > process.MaxInputBytes || len(data) == 0 && !closeInput {
			return protocol.E("invalid_params", "provide TEXT, --file, or --close; maximum input is 64 KiB")
		}
		return a.call(cmd, "process.stdin", true, process.InputParams{ID: args[0], Data: data, Close: closeInput})
	}}
	input.Flags().StringVar(&inputFile, "file", "", "Read bytes from a file, or - for stdin")
	input.Flags().BoolVar(&closeInput, "close", false, "Close pipe stdin; send EOF and disable input for canonical PTY")
	group.AddCommand(input)
	var resizeCols, resizeRows int
	resize := &cobra.Command{Use: "resize PROCESS", Short: "Resize one terminal; pipe is unsupported", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.resize", true, process.ResizeParams{ID: args[0], Cols: resizeCols, Rows: resizeRows})
	}}
	resize.Flags().IntVar(&resizeCols, "cols", 0, "Target columns (2–1000)")
	resize.Flags().IntVar(&resizeRows, "rows", 0, "Target rows (2–1000)")
	group.AddCommand(resize)
	group.AddCommand(&cobra.Command{Use: "cancel PROCESS", Short: "Send SIGINT to the owned process group(s)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.signal", true, process.SignalParams{ID: args[0], Signal: "INT"})
	}})
	var force bool
	terminate := &cobra.Command{Use: "terminate PROCESS", Short: "Send SIGTERM; --force sends SIGKILL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		signal := "TERM"
		if force {
			signal = "KILL"
		}
		return a.call(cmd, "process.signal", true, process.SignalParams{ID: args[0], Signal: signal})
	}}
	terminate.Flags().BoolVar(&force, "force", false, "Send SIGKILL to the verified owned process group(s)")
	group.AddCommand(terminate)
	group.AddCommand(&cobra.Command{Use: "control PROCESS OPERATION", Short: "Query a durable stdin, resize or signal operation", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.control.inspect", false, process.ControlTarget{ID: args[0], OperationID: args[1]})
	}})
	a.retentionCommands(group)
	root.AddCommand(group)
}

func (a *app) retentionCommands(group *cobra.Command) {
	group.AddCommand(&cobra.Command{Use: "retention", Short: "Query output storage, protected bytes and cache limits", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.call(cmd, "process.output.retention", false, struct{}{})
	}})
	for _, hold := range []bool{true, false} {
		name, description := "pin", "Keep process output while a reference is held"
		if !hold {
			name, description = "unpin", "Release one output reference; does not delete output"
		}
		var ref, kind, owner string
		command := &cobra.Command{Use: name + " PROCESS", Short: description, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return a.call(cmd, "process.output.reference", true, process.ReferenceParams{ID: args[0], ReferenceID: ref, Kind: kind, Owner: owner, Hold: hold})
		}}
		command.Flags().StringVar(&ref, "reference", "", "Stable reference ID (required)")
		command.Flags().StringVar(&kind, "kind", "manual", "manual, task or history")
		command.Flags().StringVar(&owner, "owner", "user", "Reference owner; must match when releasing")
		_ = command.MarkFlagRequired("reference")
		group.AddCommand(command)
	}
	var retainBytes int64
	var dryRun, yes bool
	rotate := &cobra.Command{Use: "rotate", Short: "Remove oldest unreferenced output; retain process history", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		method := "process.output.rotate"
		if dryRun {
			method = "process.output.rotation.plan"
		} else if !yes {
			return protocol.E("invalid_params", "output deletion is irreversible; preview with --dry-run, then pass --yes")
		}
		return a.call(cmd, method, !dryRun, process.RotateParams{RetainBytes: retainBytes})
	}}
	rotate.Flags().Int64Var(&retainBytes, "retain-bytes", 0, "Target stored output bytes (required; protected output is retained)")
	rotate.Flags().BoolVar(&dryRun, "dry-run", false, "List candidates without deleting output")
	rotate.Flags().BoolVar(&yes, "yes", false, "Confirm irreversible output deletion")
	_ = rotate.MarkFlagRequired("retain-bytes")
	group.AddCommand(rotate)
	group.AddCommand(&cobra.Command{Use: "rotation OPERATION", Short: "Query a saved output rotation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "process.output.rotation.inspect", false, process.RotationTargetParams{OperationID: args[0]})
	}})
}
