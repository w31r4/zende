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
	var parent, file, mode, cwd, background, backgroundFile, workspaceMode, worktreePath string
	var budget int
	var history []string
	spawn := &cobra.Command{Use: "spawn [TEXT]", Short: "Create a persistent child Agent and accept its first task; return without waiting", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := protocol.SpawnWorkspaceMode(workspaceMode, worktreePath); err != nil {
			return err
		}
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
		if workspaceMode == "worktree" {
			p.Workspace, p.WorktreePath = workspaceMode, worktreePath
		}
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
	spawn.Flags().StringVar(&workspaceMode, "workspace", "shared", "shared cwd or a worktree at the source repository's saved HEAD")
	spawn.Flags().StringVar(&worktreePath, "worktree-path", "", "New worktree path, relative to source cwd; parent directory must exist")
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
// uncommon parameters remain in --help.
func collaborationHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{
		{Name: "agent.wait", Usage: prefix + "agent wait AGENT --task TASK [--wait-ms 1000]", Guidance: "Wait only when progress depends on that exact task; timeout does not cancel it. Child finals also return automatically through queue. Core adds observed results to the tool context when they fit; a result reference alone leaves queue delivery pending. Do not automatically acknowledge result notifications."},
		{Name: "agent.result", Usage: prefix + "agent result AGENT --task TASK [--offset N] [--limit 65536]", Guidance: "Read a completed result by UTF-8 byte pages; continue from next while more is true. Reading pages does not consume automatic result delivery."},
		{Name: "agent.send", Usage: prefix + "--request-id ID agent send AGENT --file FILE [--delivery queue|steer]", Guidance: "Send follow-up work or information to a persistent Agent. Default queue waits for its current task; steer enters at the next request boundary after tools finish, without killing them. Idle recipients start a new task. Both modes retain messages during manual pause and never resume it. Acceptance returns message_id, not completion; inspect mailbox delivery and task_state. Reuse the original request ID after uncertain delivery; do not resend with a new ID."},
		{Name: "mailbox.list", Usage: prefix + "mailbox list [--agent AGENT] [--direction inbox|outbox]", Guidance: "Query retained communication metadata; current Agent is the default inside exec_command. Use mailbox inspect MESSAGE for original text and source. Queries do not consume messages. accepted means saved, delivered means in context, processed means the receiving task completed; failed or cancelled processing stays visible in task_state."},
		{Name: "agent.spawn", Usage: prefix + "agent spawn --task-file FILE [--context all|none|last:N] [--context-budget TOKENS]", Guidance: "Delegate independent work; returns Agent/Task IDs without waiting for the answer. Defaults inherit cwd, model and current context, excluding pending calls. Budget alone selects recent whole turns; explicit ranges must fit. Add --background-file FILE or --background-history AGENT:FIRST:LAST for evidence. Use --workspace worktree --worktree-path PATH for a detached HEAD checkout, excluding uncommitted files; retain its path, no automatic merge or removal. Inspect the receipt; retry unknown creation with the original --request-id. Same input in one command deduplicates; distinct IDs create identical children."},
		{Name: "agent.submit", Usage: prefix + "--request-id ID agent submit AGENT --file FILE", Guidance: "Reuse a saved Agent and its history for another task. Returns acceptance, not completion. Explicit submit can wake a paused Agent. Inspect its snapshot or history for progress and results."},
		{Name: "agent.inspect", Usage: prefix + "agent inspect AGENT", Guidance: "Read durable tasks and their state/results without starting a model or TUI. Use agent history AGENT for cursor-paged events."},
		{Name: "agent.stop", Usage: prefix + "--request-id ID agent stop AGENT [--tree]", Guidance: "Default cancels the current task and pauses only that Agent; background processes continue. Explicit --tree also pauses descendants and terminates owned processes and pending worktree creation. Inspect operation inspect ID until stopped; unknown effects keep the tree frozen. History, queued input and files remain. Retry with the original request ID."},
		{Name: "agent.resume", Usage: prefix + "--request-id ID agent resume AGENT", Guidance: "Resume only this Agent: consume queued work or create a continuation task. Cancelled tasks and stopped processes stay ended. Unfinished tree stops block resume; inspect agent recovery AGENT for unresolved effects. Runner support is required for --tree, not for ordinary stop/resume."},
	}
}
