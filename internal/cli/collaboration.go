package cli

import (
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) collaborationCommands(agent *cobra.Command) {
	var parent, file, mode, cwd, background, backgroundFile string
	var budget int
	var history []string
	spawn := &cobra.Command{Use: "spawn [TEXT]", Short: "Create a persistent child Agent and accept its first task; return without waiting", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if file == "-" && backgroundFile == "-" {
			return protocol.E("invalid_params", "task and background cannot both read stdin")
		}
		if cmd.Flags().Changed("context-budget") && budget <= 0 {
			return protocol.E("invalid_params", "--context-budget must be positive")
		}
		if _, _, err := protocol.SpawnContextMode(mode, budget); err != nil {
			return err
		}
		input, err := readInput(cmd, args, file, true)
		if err != nil {
			return err
		}
		backgroundArgs := []string(nil)
		if cmd.Flags().Changed("background") {
			backgroundArgs = []string{background}
		}
		text, err := readInput(cmd, backgroundArgs, backgroundFile, false)
		if err != nil {
			return err
		}
		p := protocol.SpawnAgent{ParentID: parent, Input: input, Context: mode, Cwd: cwd, ContextBudget: budget, Background: text}
		// Keep pre-budget CLI retry fingerprints stable across binary upgrades.
		if p.Context == "" && budget == 0 {
			p.Context = "all"
		}
		for _, value := range history {
			parts := strings.Split(value, ":")
			if len(parts) != 3 || parts[0] == "" {
				return protocol.E("invalid_params", "--background-history requires AGENT:FIRST:LAST")
			}
			first, err1 := strconv.ParseInt(parts[1], 10, 64)
			last, err2 := strconv.ParseInt(parts[2], 10, 64)
			if err1 != nil || err2 != nil || first <= 0 || last < first {
				return protocol.E("invalid_params", "invalid background history range")
			}
			p.BackgroundHistory = append(p.BackgroundHistory, protocol.ContextRange{AgentID: parts[0], First: first, Last: last})
		}
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
		return a.call(cmd, "agent.spawn", true, p)
	}}
	spawn.Flags().StringVar(&parent, "parent", "", "Parent Agent (automatic inside exec_command)")
	spawn.Flags().StringVar(&file, "file", "", "Read task input from file, or - for stdin")
	spawn.Flags().StringVar(&file, "task-file", "", "Read task input from file, or - for stdin")
	spawn.MarkFlagsMutuallyExclusive("file", "task-file")
	spawn.Flags().StringVar(&mode, "context", "", "Inherit all (default), none or last:N user turns; never split tool pairs")
	spawn.Flags().IntVar(&budget, "context-budget", 0, "Estimated inherited-history + background token budget; without --context selects recent complete turns")
	spawn.Flags().StringVar(&background, "background", "", "Additional task background text")
	spawn.Flags().StringVar(&backgroundFile, "background-file", "", "Read additional background from a UTF-8 file, or - for stdin")
	spawn.MarkFlagsMutuallyExclusive("background", "background-file")
	spawn.Flags().StringArrayVar(&history, "background-history", nil, "Include raw context.message range AGENT:FIRST:LAST (repeatable)")
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
		{Name: "agent.spawn", Usage: prefix + "agent spawn --task-file FILE [--context all|none|last:N] [--context-budget TOKENS]", Guidance: "Delegate an independent task; returns Agent/Task IDs without waiting. Defaults inherit cwd, model and the current context view, excluding pending parent calls. Budget alone selects recent whole turns; an explicit context mode must fit. Add --background-file FILE or --background-history AGENT:FIRST:LAST for specific evidence; inspect the returned range and estimate. Same input in one command deduplicates; use distinct --request-id IDs for identical children and reuse IDs on retry."},
		{Name: "agent.submit", Usage: prefix + "--request-id ID agent submit AGENT --file FILE", Guidance: "Reuse a saved Agent and its history for another task. Returns acceptance, not completion. Explicit submit can wake a paused Agent. Inspect its snapshot or history for progress and results."},
		{Name: "agent.inspect", Usage: prefix + "agent inspect AGENT", Guidance: "Read durable tasks and their state/results without starting a model or TUI. Use agent history AGENT for cursor-paged events."},
	}
}
