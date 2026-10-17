package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
)

func (a *app) configCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "config", Short: "Inspect configuration files or refresh the running profile"}
	var live bool
	inspect := &cobra.Command{Use: "inspect", Short: "Show local values and sources; --live reads the published Core configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if live {
			return a.call(cmd, "config.inspect", false, struct{}{})
		}
		p, effective, err := a.environment()
		if err != nil {
			return err
		}
		return encode(cmd, struct {
			Paths resource.Paths `json:"paths"`
			resource.Effective
		}{p, effective})
	}}
	inspect.Flags().BoolVar(&live, "live", false, "Read the running Core's publication and last refresh error instead of local files")
	group.AddCommand(inspect)
	group.AddCommand(&cobra.Command{Use: "refresh", Short: "Prepare and publish the running profile's configuration without restarting", Long: "Reloads the user/project/local layers using the Core's original project root and environment. Checks local model settings and credential references without sending a model request. Failure retains the last publication. Existing Tasks keep their model, cwd and budget; select a new model explicitly. Use --request-id for retries and config inspect --live for the effective version. Runner settings are reported separately by runner status.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.call(cmd, "config.refresh", true, struct{}{})
	}})
	group.AddCommand(&cobra.Command{Use: "read VERSION", Short: "Read a retained configuration version without accessing credentials or sources", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "config.read", false, protocol.ConfigRead{Version: args[0]})
	}})
	var query protocol.ConfigQuery
	history := &cobra.Command{Use: "history", Short: "List retained configuration publications", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.call(cmd, "config.history", false, query)
	}}
	history.Flags().Int64Var(&query.After, "after", 0, "Continue after this publication generation")
	history.Flags().IntVar(&query.Limit, "limit", 20, "Maximum publications, 1–100")
	group.AddCommand(history)
	root.AddCommand(group)
}

func configHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " config "
	return []runtime.CommandHint{
		{Name: "config.inspect", Usage: prefix + "inspect --live", Guidance: "Read Core's published configuration, sources and last refresh error. Without --live, inspect reads files relative to this command's cwd; it does not show what the running Core uses."},
		{Name: "config.refresh", Usage: prefix + "refresh", Guidance: "After changing configuration, prepare and publish using Core's original project root/environment. Reuse --request-id on retry. Failure keeps the last version. Discover new connections with model connections/list, then explicitly select them with agent select; refresh does not change this Task's model, cwd or budget."},
	}
}
