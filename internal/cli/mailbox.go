package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
)

func (a *app) sendCommand(agent *cobra.Command) {
	var file, delivery string
	cmd := &cobra.Command{Use: "send AGENT [TEXT]", Short: "Save a mailbox message without waiting for delivery or waking a pause", Long: "Save up to 256 KiB UTF-8 text; at most 128 messages may await delivery per recipient. Returns message_id, not processing completion. Resume handles already waiting messages in receipt order.", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		if delivery != "queue" && delivery != "steer" {
			return protocol.E("invalid_params", "delivery must be queue or steer")
		}
		input, err := readInput(cmd, args[1:], file, true)
		if err != nil {
			return err
		}
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		return a.call(cmd, "agent.send", true, protocol.SendMessage{AgentID: args[0], Input: input, Delivery: delivery, Caller: caller})
	}}
	cmd.Flags().StringVar(&file, "file", "", "Read up to 256 KiB UTF-8 text from file, or - for stdin")
	cmd.Flags().StringVar(&delivery, "delivery", "queue", "queue after the current task, or steer at the next model-request boundary")
	agent.AddCommand(cmd)
}

func (a *app) mailboxCommands(root *cobra.Command) {
	cmd := &cobra.Command{Use: "mailbox", Short: "Query retained messages, delivery and receiving task outcomes"}
	var p protocol.MailboxQuery
	list := &cobra.Command{Use: "list", Short: "List metadata; use inspect for message text", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		query := p
		if query.AgentID == "" {
			caller, err := callerFromEnvironment()
			if err != nil {
				return err
			}
			if caller != nil {
				query.AgentID = caller.AgentID
			}
		}
		if query.AgentID == "" {
			return protocol.E("invalid_params", "--agent is required outside an Agent command")
		}
		return a.call(cmd, "mailbox.list", false, query)
	}}
	list.Flags().StringVar(&p.AgentID, "agent", "", "Recipient for inbox, sender for outbox (automatic inside exec_command)")
	list.Flags().StringVar(&p.Direction, "direction", "inbox", "inbox or outbox")
	list.Flags().StringVar(&p.State, "state", "all", "all, accepted, delivered or processed; task_state reports failures separately")
	list.Flags().Int64Var(&p.After, "after", 0, "Agent event cursor from the previous page")
	list.Flags().IntVar(&p.Limit, "limit", 50, "Maximum metadata records (1–100)")
	cmd.AddCommand(list)
	cmd.AddCommand(&cobra.Command{Use: "inspect MESSAGE", Short: "Read original message, source, delivery cursor and receiving task state", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "mailbox.inspect", false, protocol.MailTarget{ID: args[0]})
	}})
	root.AddCommand(cmd)
}
