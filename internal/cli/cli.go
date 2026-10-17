// Package cli is a short-lived client of the durable core.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/mcp"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type BuildInfo struct{ Version, Commit, Date string }
type app struct {
	profile, operationID string
	jsonOutput           bool
	build                BuildInfo
}

func Run(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer, build BuildInfo) int {
	a := &app{build: build}
	root := a.command()
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(diagnostic)
	if err := root.ExecuteContext(ctx); err != nil {
		code := 1
		var wire *protocol.Error
		if errors.As(err, &wire) {
			json.NewEncoder(diagnostic).Encode(wire)
			if wire.Data.Kind == "invalid_params" || wire.Data.Kind == "invalid_request" {
				code = 2
			}
		} else {
			fmt.Fprintln(diagnostic, err)
		}
		return code
	}
	return 0
}

func (a *app) command() *cobra.Command {
	root := &cobra.Command{Use: "zende", Short: "Persistent terminal agent harness", SilenceErrors: true, SilenceUsage: true,
		Version: fmt.Sprintf("%s (%s, %s; %s)", a.build.Version, a.build.Commit, a.build.Date, goruntime.Version()),
		RunE:    func(cmd *cobra.Command, args []string) error { return cmd.Help() }, Args: cobra.NoArgs}
	root.PersistentFlags().StringVar(&a.profile, "profile", "default", "Isolated configuration profile")
	root.PersistentFlags().StringVar(&a.operationID, "request-id", "", "Stable mutation ID; reuse when retrying")
	root.PersistentFlags().BoolVar(&a.jsonOutput, "json", false, "JSON results (the default for non-interactive commands)")
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print build version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { fmt.Fprintln(cmd.OutOrStdout(), root.Version); return nil }})
	core := &cobra.Command{Use: "core", Short: "Manage the profile's background core"}
	core.AddCommand(&cobra.Command{Use: "serve", Short: "Run core in foreground (supervisor entry)", Args: cobra.NoArgs, RunE: a.serve})
	core.AddCommand(&cobra.Command{Use: "start", Short: "Start a detached core or connect to the existing one", Args: cobra.NoArgs, RunE: a.start})
	core.AddCommand(&cobra.Command{Use: "status", Short: "Inspect core identity and limits", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.call(cmd, "core.status", false, struct{}{}) }})
	root.AddCommand(core)
	runner := &cobra.Command{Use: "runner", Short: "Independent process runner"}
	runner.AddCommand(&cobra.Command{Use: "serve", Short: "Run the process supervisor in foreground", Args: cobra.NoArgs, RunE: a.serveRunner})
	runner.AddCommand(&cobra.Command{Use: "start", Short: "Start a detached runner", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.startService(cmd, "runner") }})
	runner.AddCommand(&cobra.Command{Use: "status", Short: "Inspect runner identity and limits", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.call(cmd, "runner.status", false, struct{}{}) }})
	root.AddCommand(runner)
	a.processCommands(root)
	a.tuiCommands(root)
	a.attachmentCommands(root)
	a.questionCommands(root)
	a.mailboxCommands(root)
	a.resourceCommands(root)
	a.workspaceCommands(root)
	a.contextCommands(root)
	config := &cobra.Command{Use: "config", Short: "Inspect effective configuration without resolving secrets"}
	config.AddCommand(&cobra.Command{Use: "inspect", Short: "Show values and override sources", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		p, e, err := a.environment()
		if err != nil {
			return err
		}
		return encode(cmd, struct {
			Paths resource.Paths `json:"paths"`
			resource.Effective
		}{p, e})
	}})
	root.AddCommand(config)
	agent := &cobra.Command{Use: "agent", Short: "Persistent agent sessions"}
	var createCwd, file string
	create := &cobra.Command{Use: "create [TEXT]", Short: "Create a session and optionally accept an input", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := readInput(cmd, args, file, false)
		if err != nil {
			return err
		}
		cwd := createCwd
		if cwd == "" {
			cwd, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return err
		}
		return a.call(cmd, "agent.create", true, protocol.CreateAgent{Cwd: cwd, Input: input})
	}}
	create.Flags().StringVar(&createCwd, "cwd", "", "Initial working directory")
	create.Flags().StringVar(&file, "file", "", "Read text from file, or - for stdin")
	agent.AddCommand(create)
	var submitFile string
	var submitImages []string
	var submitSkills []string
	submit := &cobra.Command{Use: "submit AGENT [TEXT]", Short: "Durably accept a task input", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := readInput(cmd, args[1:], submitFile, len(submitImages) == 0 && len(submitSkills) == 0)
		if err != nil {
			return err
		}
		skills, err := parseSkillRefs(submitSkills)
		if err != nil {
			return err
		}
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		return a.call(cmd, "agent.submit", true, protocol.Submit{AgentID: args[0], Input: input, Attachments: submitImages, Skills: skills, Caller: caller})
	}}
	submit.Flags().StringVar(&submitFile, "file", "", "Read text from file, or - for stdin")
	submit.Flags().StringArrayVar(&submitImages, "image", nil, "Imported image ID (repeatable); bytes remain in profile storage")
	submit.Flags().StringArrayVar(&submitSkills, "skill", nil, "Published RESOURCE@VERSION (repeatable); use the exact selected instructions")
	agent.AddCommand(submit)
	for _, action := range []string{"stop", "resume"} {
		short := "Cancel the current task and pause this Agent; background processes continue"
		if action == "resume" {
			short = "Resume queued work or create a continuation task; cancelled tasks stay cancelled"
		}
		var tree bool
		control := &cobra.Command{Use: action + " AGENT", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var params any = protocol.Target{AgentID: args[0]}
			if action == "stop" {
				params = protocol.StopAgent{AgentID: args[0], Tree: tree}
			}
			return a.call(cmd, "agent."+action, true, params)
		}}
		if action == "stop" {
			control.Flags().BoolVar(&tree, "tree", false, "Also pause descendants and terminate their owned processes and pending worktree creation; retain history and files")
			control.Long = short + ".\nWith --tree, core freezes the captured tree and verifies termination asynchronously.\nAcceptance is not completion: use operation inspect ID; unresolved effects keep the tree paused."
		}
		agent.AddCommand(control)
	}
	agent.AddCommand(&cobra.Command{Use: "list", Short: "List saved sessions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.call(cmd, "agent.list", false, struct{}{}) }})
	agent.AddCommand(&cobra.Command{Use: "activity AGENT", Short: "List active Agent-tree work; mark unavailable sources and truncation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.activity", false, protocol.Target{AgentID: args[0]})
	}})
	agent.AddCommand(&cobra.Command{Use: "inspect AGENT", Short: "Read a consistent session snapshot and cursor", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.inspect", false, protocol.Target{AgentID: args[0]})
	}})
	var after int64
	var limit int
	history := &cobra.Command{Use: "history AGENT", Short: "Read append-only history after a cursor", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.history", false, protocol.Cursor{AgentID: args[0], After: after, Limit: limit})
	}}
	history.Flags().Int64Var(&after, "after", 0, "Last consumed event sequence")
	history.Flags().IntVar(&limit, "limit", 100, "Maximum events (1–1000)")
	agent.AddCommand(history)
	var recoveryTask string
	recovery := &cobra.Command{Use: "recovery AGENT", Short: "Inspect recovery facts without executing or resuming work", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.recovery", false, protocol.RecoveryTarget{AgentID: args[0], TaskID: recoveryTask})
	}}
	recovery.Flags().StringVar(&recoveryTask, "task", "", "Task identity (default: unfinished or latest task)")
	agent.AddCommand(recovery)
	var recoverTask string
	recoverCommand := &cobra.Command{Use: "recover AGENT", Short: "Verify original effects and continue a held task when safe", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.recover", true, protocol.RecoveryTarget{AgentID: args[0], TaskID: recoverTask})
	}}
	recoverCommand.Flags().StringVar(&recoverTask, "task", "", "Held task identity")
	agent.AddCommand(recoverCommand)
	a.selectionCommands(agent)
	a.collaborationCommands(agent)
	a.sendCommand(agent)
	a.resultCommands(agent)
	root.AddCommand(agent)
	operation := &cobra.Command{Use: "operation", Short: "Look up acceptance after a connection failure"}
	operation.AddCommand(&cobra.Command{Use: "inspect ID", Short: "Inspect a stable mutation request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "operation.inspect", false, map[string]string{"operation_id": args[0]})
	}})
	root.AddCommand(operation)
	events := &cobra.Command{Use: "events", Short: "Observe semantic events without resubmitting work"}
	var eventAgent string
	var eventAfter int64
	watch := &cobra.Command{Use: "watch", Short: "Stream NDJSON events until interrupted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if eventAgent == "" {
			return protocol.E("invalid_params", "--agent is required")
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		return (protocol.Client{Socket: p.Socket}).Watch(cmd.Context(), protocol.Cursor{AgentID: eventAgent, After: eventAfter}, func(e protocol.Event) error { return encode(cmd, e) })
	}}
	watch.Flags().StringVar(&eventAgent, "agent", "", "Agent identity")
	watch.Flags().Int64Var(&eventAfter, "after", 0, "Last consumed sequence")
	events.AddCommand(watch)
	root.AddCommand(events)
	return root
}

func encode(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

func (a *app) paths() (resource.Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return resource.Paths{}, err
	}
	return resource.ResolvePaths(a.profile, home, os.Getenv)
}

func (a *app) environment() (resource.Paths, resource.Effective, error) {
	p, err := a.paths()
	if err != nil {
		return p, resource.Effective{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return p, resource.Effective{}, err
	}
	e, err := resource.Load(p, cwd, os.Getenv, nil, nil)
	return p, e, err
}

func (a *app) call(cmd *cobra.Command, method string, mutation bool, params any) error {
	p, err := a.paths()
	if err != nil {
		return err
	}
	op := ""
	if mutation {
		op = a.operationID
		if op == "" {
			op = protocol.NewID("op")
		}
	}
	req, err := protocol.NewRequest(method, op, params)
	if err != nil {
		return err
	}
	var caller *protocol.Caller
	switch p := params.(type) {
	case protocol.SpawnAgent:
		caller = p.Caller
	case protocol.Submit:
		caller = p.Caller
	case protocol.SendMessage:
		caller = p.Caller
	case protocol.PluginCommand:
		caller = p.Caller
	case protocol.PluginSubscribe:
		caller = p.Caller
	case protocol.CompactContext:
		caller = p.Caller
	}
	if caller != nil && a.operationID == "" {
		// One execution may retry the same CLI after losing stdout or reconnecting.
		// Include caller identity and all arguments, not the short-lived RPC ID.
		fingerprint, err := req.Fingerprint()
		if err != nil {
			return err
		}
		req.OperationID = "call_" + fingerprint
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout(method, params))
	defer cancel()
	var result json.RawMessage
	socket := p.Socket
	if strings.HasPrefix(method, "runner.") || strings.HasPrefix(method, "process.") {
		socket = p.RunnerSocket
	}
	err = (protocol.Client{Socket: socket}).Retry(ctx, req, &result, 3, 100*time.Millisecond)
	if err != nil {
		return err
	}
	return encode(cmd, result)
}

func requestTimeout(method string, params any) time.Duration {
	if p, ok := params.(protocol.WaitTask); ok {
		return time.Duration(p.WaitMS)*time.Millisecond + 5*time.Second
	}
	switch method {
	case "agent.spawn":
		// A selected plugin model must prepare an independent child instance.
		return plugin.InitializeTimeout + 10*time.Second
	case "resource.register", "resource.refresh", "mcp.search":
		// MCP initialization/catalog refresh may consume the full server budget.
		// Leave time for receipt persistence and process cleanup before replying.
		return mcp.RefreshTimeout + 10*time.Second
	default:
		return 10 * time.Second
	}
}

func (a *app) serve(cmd *cobra.Command, _ []string) error {
	p, e, err := a.environment()
	if err != nil {
		return err
	}
	if err := p.EnsureState(); err != nil {
		return err
	}
	// Bind/lock before opening the database so only the elected core migrates it.
	proxy := &deferredHandler{}
	server, err := protocol.Listen(p.Socket, proxy)
	if err != nil {
		return err
	}
	s, err := store.Open(p.Database)
	if err != nil {
		ctx, cancel := context.WithCancel(cmd.Context())
		cancel()
		server.Serve(ctx)
		return err
	}
	defer s.Close()
	core := runtime.New(s, e)
	if exe, err := os.Executable(); err == nil {
		core.Commands = append(questionHints(exe, a.profile), resourceHints(exe, a.profile)...)
		core.Commands = append(core.Commands, workspaceHints(exe, a.profile)...)
		core.Commands = append(core.Commands, selectionHints(exe, a.profile)...)
		core.Commands = append(core.Commands, collaborationHints(exe, a.profile)...)
		core.Commands = append(core.Commands, contextHints(exe, a.profile)...)
	}
	configuredModels, err := provider.New(e.Config, os.Getenv)
	if err != nil {
		ctx, cancel := context.WithCancel(cmd.Context())
		cancel()
		_ = server.Serve(ctx)
		return err
	}
	core.RestoreModels = configuredModels
	core.Catalog = configuredModels
	if e.Config.Model != "" {
		core.Models = configuredModels
	}
	runner := process.Remote{Client: protocol.Client{Socket: p.RunnerSocket}}
	core.Runner = runner
	executors := append(tool.ProcessExecutors(runner), tool.DurablePatchExecutor(filepath.Join(p.State, "patches")))
	executors = append(executors, core.MCPExecutors()...)
	core.Tools, err = tool.NewRegistry(append(executors, core.PluginExecutors()...)...)
	if err != nil {
		ctx, cancel := context.WithCancel(cmd.Context())
		cancel()
		_ = server.Serve(ctx)
		return err
	}
	proxy.handler = core
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	finished := make(chan error, 2)
	go func() { finished <- core.Run(ctx) }()
	go func() { finished <- server.Serve(ctx) }()
	err = <-finished
	cancel()
	if other := <-finished; err == nil {
		err = other
	}
	return err
}

type deferredHandler struct{ handler protocol.Handler }

func (d *deferredHandler) Handle(ctx context.Context, r protocol.Request) (any, error) {
	return d.handler.Handle(ctx, r)
}
func (d *deferredHandler) Subscribe(ctx context.Context, r protocol.Request, f func(protocol.Event) error) error {
	return d.handler.Subscribe(ctx, r, f)
}

func (a *app) start(cmd *cobra.Command, _ []string) error {
	return a.startService(cmd, "core")
}

func (a *app) startService(cmd *cobra.Command, service string) error {
	p, err := a.paths()
	if err != nil {
		return err
	}
	socket := p.Socket
	if service == "runner" {
		socket = p.RunnerSocket
	}
	client := protocol.Client{Socket: socket}
	status := func() (json.RawMessage, error) {
		req, _ := protocol.NewRequest(service+".status", "", struct{}{})
		ctx, cancel := context.WithTimeout(cmd.Context(), 500*time.Millisecond)
		defer cancel()
		var out json.RawMessage
		err := client.Call(ctx, req, &out)
		return out, err
	}
	if result, err := status(); err == nil {
		return encode(cmd, result)
	}
	if _, _, err := a.environment(); err != nil {
		return err
	}
	if err := p.EnsureState(); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(p.State, service+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(exe, "--profile", a.profile, service, "serve")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout = log
	child.Stderr = log
	if err := child.Start(); err != nil {
		return err
	}
	child.Process.Release()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-timer.C:
			return protocol.E("unavailable", service+" did not become ready; inspect the profile "+service+".log")
		case <-ticker.C:
			if result, err := status(); err == nil {
				return encode(cmd, result)
			}
		}
	}
}

func readInput(cmd *cobra.Command, args []string, file string, required bool) (string, error) {
	if file != "" && len(args) > 0 {
		return "", protocol.E("invalid_params", "choose either TEXT or --file")
	}
	if file == "" {
		if len(args) == 1 {
			if !utf8.ValidString(args[0]) || len(args[0]) > 2<<20 || (required && args[0] == "") {
				return "", protocol.E("invalid_params", "input must be nonempty UTF-8 and at most 2 MiB")
			}
			return args[0], nil
		}
		if required {
			return "", protocol.E("invalid_params", "TEXT or --file is required")
		}
		return "", nil
	}
	r := cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("cannot open input file")
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 2<<20+1))
	if err != nil {
		return "", fmt.Errorf("cannot read input")
	}
	if len(b) > 2<<20 || !utf8.Valid(b) {
		return "", protocol.E("invalid_params", "input must be UTF-8 and at most 2 MiB")
	}
	if required && len(b) == 0 {
		return "", protocol.E("invalid_params", "input is empty")
	}
	return string(b), nil
}
