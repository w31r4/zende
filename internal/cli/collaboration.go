package cli

import (
	"os"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) collaborationCommands(agent *cobra.Command) {
	var parent, file, mode, cwd string
	spawn := &cobra.Command{Use: "spawn [TEXT]", Short: "Create a persistent child Agent and accept its first task; return without waiting", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := readInput(cmd, args, file, true)
		if err != nil {
			return err
		}
		p := protocol.SpawnAgent{ParentID: parent, Input: input, Context: mode, Cwd: cwd}
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		if caller != nil {
			p.Caller = caller
			if p.ParentID == "" {
				p.ParentID = caller.AgentID
			}
			if p.ParentID != caller.AgentID {
				return protocol.E("invalid_params", "--parent must match the command's caller")
			}
		}
		if p.ParentID == "" {
			return protocol.E("invalid_params", "--parent is required outside an Agent command")
		}
		if mode != "all" && mode != "none" {
			return protocol.E("invalid_params", "--context must be all or none")
		}
		return a.call(cmd, "agent.spawn", true, p)
	}}
	spawn.Flags().StringVar(&parent, "parent", "", "Parent Agent (automatic inside exec_command)")
	spawn.Flags().StringVar(&file, "file", "", "Read task input from file, or - for stdin")
	spawn.Flags().StringVar(&file, "task-file", "", "Read task input from file, or - for stdin")
	spawn.MarkFlagsMutuallyExclusive("file", "task-file")
	spawn.Flags().StringVar(&mode, "context", "all", "Inherit complete parent context: all or none")
	spawn.Flags().StringVar(&cwd, "cwd", "", "Existing directory relative to parent cwd; default inherits parent cwd")
	agent.AddCommand(spawn)
}

func callerFromEnvironment() (*protocol.Caller, error) {
	raw := os.Getenv(protocol.CallerEnvironment)
	if raw == "" {
		return nil, nil
	}
	var caller protocol.Caller
	if len(raw) > 4096 || protocol.StrictDecode([]byte(raw), &caller) != nil || caller.AgentID == "" || caller.TaskID == "" || caller.OperationID == "" {
		return nil, protocol.E("invalid_params", "invalid runner caller metadata")
	}
	return &caller, nil
}

// Pi-style, capability-conditioned one-line usage: common defaults are visible,
// uncommon parameters remain in --help. No unregistered send/wait promise.
func collaborationHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{
		{Name: "agent.spawn", Usage: prefix + "agent spawn --task-file FILE [--context all|none]", Guidance: "Delegate a concrete independent task. Returns Agent and Task IDs immediately; no TUI opens. Caller, cwd, selected model and complete context are inherited by default; pending parent calls are excluded and reported. Same input in the same command deduplicates automatically; use distinct --request-id IDs for intentionally identical children, and reuse each ID on retry."},
		{Name: "agent.submit", Usage: prefix + "--request-id ID agent submit AGENT --file FILE", Guidance: "Reuse a saved Agent and its history for another task. Returns acceptance, not completion. Explicit submit can wake a paused Agent. Inspect its snapshot or history for progress and results."},
		{Name: "agent.inspect", Usage: prefix + "agent inspect AGENT", Guidance: "Read durable tasks and their state/results without starting a model or TUI. Use agent history AGENT for cursor-paged events."},
	}
}
