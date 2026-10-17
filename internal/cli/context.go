package cli

import (
	"strconv"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func contextTargetFlags(cmd *cobra.Command, target *protocol.ContextTarget) func() error {
	var at int64
	cmd.Flags().Int64Var(&at, "at", 0, "Fixed event boundary from cursor; omitted selects current history")
	cmd.Flags().StringVar(&target.RequestID, "request", "", "Exact saved model request instead of --at")
	return func() error {
		target.At = nil
		if cmd.Flags().Changed("at") {
			if at < 0 || cmd.Flags().Changed("request") {
				return protocol.E("invalid_params", "--at must be nonnegative and cannot be combined with --request")
			}
			target.At = &at
		}
		if cmd.Flags().Changed("request") && target.RequestID == "" {
			return protocol.E("invalid_params", "--request requires a model request ID")
		}
		return nil
	}
}

func (a *app) contextCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "context", Short: "Inspect retained context or request a durable summary"}
	group.AddCommand(&cobra.Command{Use: "compact AGENT", Short: "Queue compaction at the next safe boundary; return its operation ID", Long: "Accepts a bounded summary operation without waiting for the model. Busy Agents handle it after their current tools and pending answers; idle or paused Agents summarize without starting a Task or resuming. Inspect the returned operation_id with operation inspect. Reuse --request-id on retry; stop cancels the pending summary. Original history is retained.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		return a.call(cmd, "context.compact", true, protocol.CompactContext{AgentID: args[0], Caller: caller})
	}})
	var target protocol.ContextTarget
	inspect := &cobra.Command{Use: "inspect AGENT", Short: "Inspect a fixed context boundary and resource versions", Args: cobra.ExactArgs(1)}
	prepareInspect := contextTargetFlags(inspect, &target)
	inspect.RunE = func(cmd *cobra.Command, args []string) error {
		if err := prepareInspect(); err != nil {
			return err
		}
		target.AgentID = args[0]
		return a.call(cmd, "context.inspect", false, target)
	}
	group.AddCommand(inspect)

	var p protocol.ContextRead
	read := &cobra.Command{Use: "read AGENT", Short: "Page effective context, or original messages with --raw", Long: "Returns public context JSON without provider-private replay bytes. Keep the returned cursor as --at, or reuse --request, when continuing with --after next. An item with more=true is a complete reference, not truncated text: read it using context entry AGENT SEQ.", Args: cobra.ExactArgs(1)}
	prepareRead := contextTargetFlags(read, &p.ContextTarget)
	read.Flags().BoolVar(&p.Raw, "raw", false, "Read original committed messages, including summarized history")
	read.Flags().Int64Var(&p.After, "after", 0, "Last consumed position from next; requires --at or --request")
	read.Flags().IntVar(&p.Limit, "limit", 20, "Maximum entries per page (1–100)")
	read.Flags().IntVar(&p.MaxBytes, "max-bytes", 256<<10, "Maximum JSON page bytes (4096–4194304)")
	read.RunE = func(cmd *cobra.Command, args []string) error {
		if err := prepareRead(); err != nil {
			return err
		}
		if p.After < 0 || p.Limit < 1 || p.Limit > 100 || p.MaxBytes < 4096 || p.MaxBytes > 4<<20 || p.After > 0 && p.At == nil && p.RequestID == "" {
			return protocol.E("invalid_params", "use a nonnegative --after with --at/--request, --limit 1..100 and --max-bytes 4096..4194304")
		}
		p.AgentID = args[0]
		return a.call(cmd, "context.read", false, p)
	}
	group.AddCommand(read)

	var entry protocol.ContextEntryRead
	readEntry := &cobra.Command{Use: "entry AGENT SEQ", Short: "Read one immutable context entry by UTF-8 byte pages", Long: "Concatenate text in offset order until more=false to recover the public JSON entry. bytes and sha256 describe that complete public representation; private replay data is retained in storage and excluded here. The same entry also reads prompt_seq returned by context inspect --request.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		seq, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || seq < 1 || entry.Offset < 0 || entry.Limit < 4 || entry.Limit > 256<<10 {
			return protocol.E("invalid_params", "entry requires a positive SEQ, nonnegative --offset and --limit 4..262144")
		}
		entry.AgentID, entry.Seq = args[0], seq
		return a.call(cmd, "context.entry", false, entry)
	}}
	readEntry.Flags().IntVar(&entry.Offset, "offset", 0, "UTF-8 byte cursor from next")
	readEntry.Flags().IntVar(&entry.Limit, "limit", 64<<10, "Maximum bytes per page (4–262144)")
	group.AddCommand(readEntry)
	root.AddCommand(group)
}

// Follow Pi's short, capability-gated CLI hints. Detailed flags stay in help.
func contextHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " context "
	return []runtime.CommandHint{
		{Name: "context.compact", Usage: prefix + "compact AGENT", Guidance: "Request a summary when earlier context can be condensed. Returns operation_id; query it with operation inspect ID. For your own Agent, return from this command without polling: core summarizes after current tools and pending answers settle. History stays readable; compaction does not resume a paused Agent."},
		{Name: "context.inspect", Usage: prefix + "inspect AGENT [--request REQUEST]", Guidance: "Inspect the current view or one model request's saved boundary, prompt reference, tool versions and resource catalog. A catalog lists available definitions, not instructions already applied."},
		{Name: "context.read", Usage: prefix + "read AGENT [--raw] [--at CURSOR --after NEXT]", Guidance: "Look up details omitted by a summary. --raw reads original committed messages; default reads the summary and retained tail. Keep cursor as --at across pages. If an item has more=true, use context entry AGENT SEQ and byte-page text until more=false. Queries do not change history or run work."},
	}
}
