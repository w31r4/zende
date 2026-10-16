package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
)

func (a *app) pluginEventCommands(parent *cobra.Command) {
	events := &cobra.Command{Use: "events", Short: "Subscribe, inspect and close plugin event streams"}
	var input protocol.PluginSubscribe
	subscribe := &cobra.Command{Use: "subscribe AGENT RESOURCE REF", Short: "Accept a background subscription bound to the published event definition", Long: "REF comes from plugin catalog. Direction core delivers this Agent's original events; direction plugin retains plugin data for explicit read. The --request-id is also the subscription ID: reuse it on retry. Exit 0 means accepted, not opened. Inspect for open/closed/unknown; an unknown subscription is never reopened automatically. Disconnecting or ordinary Task stop preserves the subscription; explicit tree stop closes it. Core owns the working directory and verifies ZENDE_CALLER when present.", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		input.AgentID, input.ResourceID, input.DefinitionRef, input.Caller = args[0], args[1], args[2], caller
		return a.call(cmd, "plugin.events.subscribe", true, input)
	}}
	subscribe.Flags().Int64Var(&input.After, "after", 0, "Core source sequence to start after; plugin sources require 0")
	subscribe.Flags().StringArrayVar(&input.Kinds, "kind", nil, "Core event kind to include; repeat for multiple kinds; default is all")
	events.AddCommand(subscribe)
	for _, action := range []string{"inspect", "close"} {
		events.AddCommand(&cobra.Command{Use: action + " AGENT SUBSCRIPTION", Short: map[string]string{"inspect": "Read the retained binding, cursor and state", "close": "Request closure; inspect for confirmation, using an independent --request-id"}[action], Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			return a.call(cmd, "plugin.events."+action, action == "close", protocol.PluginSubscriptionTarget{AgentID: args[0], SubscriptionID: args[1]})
		}})
	}
	var query protocol.PluginSubscriptionQuery
	list := &cobra.Command{Use: "list AGENT", Short: "List saved subscriptions without contacting plugins", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		query.AgentID = args[0]
		return a.call(cmd, "plugin.events.list", false, query)
	}}
	list.Flags().StringVar(&query.ResourceID, "resource", "", "Filter by plugin resource ID")
	list.Flags().StringVar(&query.After, "after", "", "Previous page's next subscription ID")
	list.Flags().IntVar(&query.Limit, "limit", 20, "Maximum subscriptions (1–100)")
	events.AddCommand(list)
	var read protocol.PluginEventsRead
	readCmd := &cobra.Command{Use: "read AGENT SUBSCRIPTION", Short: "Read retained plugin data with its binding; does not contact the plugin", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		read.AgentID, read.SubscriptionID = args[0], args[1]
		return a.call(cmd, "plugin.events.read", false, read)
	}}
	readCmd.Flags().Int64Var(&read.After, "after", 0, "Sequence from previous next; default starts at the beginning")
	readCmd.Flags().IntVar(&read.Limit, "limit", 16, "Maximum events (1–64), also bounded by the response byte budget")
	events.AddCommand(readCmd)
	parent.AddCommand(events)
}
