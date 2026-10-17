package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) selectionCommands(agent *cobra.Command) {
	agent.AddCommand(&cobra.Command{Use: "selection AGENT", Short: "Read desired cwd, model and effort without resolving credentials", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.selection", false, protocol.Target{AgentID: args[0]})
	}})
	var cwd, model, effort, version, contextMode string
	var revision int64
	var preview bool
	selectCommand := &cobra.Command{Use: "select AGENT", Short: "Persist settings for the next model request; existing calls keep their settings", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p := protocol.SelectAgent{AgentID: args[0], CatalogVersion: version}
		if cmd.Flags().Changed("cwd") {
			p.Cwd = &cwd
		}
		if cmd.Flags().Changed("model") {
			p.Model = &model
		}
		if cmd.Flags().Changed("effort") {
			p.Effort = &effort
		}
		if cmd.Flags().Changed("context") {
			p.ContextMode = &contextMode
		}
		if cmd.Flags().Changed("expected-revision") {
			p.ExpectedRevision = &revision
		}
		if preview {
			return a.call(cmd, "agent.select.preview", false, p)
		}
		return a.call(cmd, "agent.select", true, p)
	}}
	selectCommand.Flags().StringVar(&cwd, "cwd", "", "Existing directory, relative to the current Agent cwd")
	selectCommand.Flags().StringVar(&model, "model", "", "Configured CONNECTION/MODEL")
	selectCommand.Flags().StringVar(&effort, "effort", "", "Model-supported effort; empty uses provider default (requires --model)")
	selectCommand.Flags().StringVar(&contextMode, "context", "strict", "History mode: strict or public; public exports compatible public content and retains original history (requires --model)")
	selectCommand.Flags().StringVar(&version, "catalog-version", "", "Require an unchanged model catalog (requires --model)")
	selectCommand.Flags().Int64Var(&revision, "expected-revision", 0, "Require the revision returned by agent selection")
	selectCommand.Flags().BoolVar(&preview, "preview", false, "Read capability differences and local compatibility without selecting or contacting the model (requires --model)")
	agent.AddCommand(selectCommand)
}

func selectionHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{
		{Name: "agent.selection", Usage: prefix + "agent selection AGENT", Guidance: "Read desired cwd, model, effort, context mode and revision. An in-flight request may still use the previous selection."},
		{Name: "agent.select", Usage: prefix + "agent select AGENT [--cwd PATH] [--model CONNECTION/MODEL --effort VALUE --context strict|public]", Guidance: "Persist settings for the next request. Discover configured models first. Add --preview to check differences and context compatibility locally. Each model selection defaults to strict continuation; use --context public explicitly to export validated public history while retaining originals. Unsupported content still fails. Empty effort uses the provider default. Existing calls keep their bindings. Use --expected-revision to fence another selection."},
	}
}
