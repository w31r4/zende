package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) workspaceCommands(root *cobra.Command) {
	workspace := &cobra.Command{Use: "workspace", Short: "Read directories and Git status without changing the session"}
	var after string
	var limit int
	directory := &cobra.Command{Use: "directory AGENT [PATH]", Short: "List a page of child directories, relative to the Agent cwd", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		p := protocol.DirectoryQuery{AgentID: args[0], After: after, Limit: limit}
		if len(args) == 2 {
			p.Path = args[1]
		}
		return a.call(cmd, "workspace.directory", false, p)
	}}
	directory.Flags().StringVar(&after, "after", "", "Previous page's next cursor")
	directory.Flags().IntVar(&limit, "limit", 40, "Maximum directories (1–100)")
	workspace.AddCommand(directory)
	workspace.AddCommand(&cobra.Command{Use: "git AGENT [PATH]", Short: "Read local Git branch and change counts; no fetch or index refresh", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		p := protocol.GitQuery{AgentID: args[0]}
		if len(args) == 2 {
			p.Path = args[1]
		}
		return a.call(cmd, "workspace.git", false, p)
	}})
	root.AddCommand(workspace)

	model := &cobra.Command{Use: "model", Short: "Inspect model catalogs; credentials remain unresolved"}
	var connectionQuery protocol.ConnectionQuery
	connections := &cobra.Command{Use: "connections", Short: "List connections and installed protocol support (not network health)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.call(cmd, "model.connections", false, connectionQuery)
	}}
	connections.Flags().StringVar(&connectionQuery.AgentID, "agent", "", "Include this Agent's plugin providers")
	connections.Flags().StringVar(&connectionQuery.After, "after", "", "Previous page's next cursor")
	connections.Flags().IntVar(&connectionQuery.Limit, "limit", 20, "Maximum connections (1–100)")
	connections.Flags().StringVar(&connectionQuery.Version, "version", "", "Require an unchanged catalog version")
	model.AddCommand(connections)
	var modelQuery protocol.ModelQuery
	list := &cobra.Command{Use: "list CONNECTION", Short: "List models, context limits, images and supported efforts", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p := modelQuery
		p.Connection = args[0]
		return a.call(cmd, "model.list", false, p)
	}}
	list.Flags().StringVar(&modelQuery.AgentID, "agent", "", "Include this Agent's plugin providers")
	list.Flags().StringVar(&modelQuery.After, "after", "", "Previous page's next cursor")
	list.Flags().IntVar(&modelQuery.Limit, "limit", 20, "Maximum models (1–100)")
	list.Flags().StringVar(&modelQuery.Version, "version", "", "Require an unchanged catalog version")
	model.AddCommand(list)
	root.AddCommand(model)
}

func workspaceHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{
		{Name: "workspace.directory", Usage: prefix + "workspace directory AGENT [PATH]", Guidance: "Browse directories relative to the Agent cwd without changing it. Use --after NEXT for another page. Shell commands may still inspect files directly."},
		{Name: "workspace.git", Usage: prefix + "workspace git AGENT [PATH]", Guidance: "Read local branch and change counts without fetching or updating the index. Counts exclude submodule internals; an untracked directory counts once."},
		{Name: "model.connections", Usage: prefix + "model connections [--agent AGENT]", Guidance: "Inspect configured connections; add --agent AGENT to include that Agent's plugin providers. Use model list CONNECTION with the same --agent for models and efforts. Supported means an installed adapter, not verified credentials or endpoint health."},
	}
}
