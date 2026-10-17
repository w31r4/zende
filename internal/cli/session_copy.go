package cli

import (
	"io"
	"os"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) copyCommands(agent *cobra.Command) {
	var cwd, drafts string
	var confirm bool
	var revision int64
	command := &cobra.Command{Use: "copy AGENT", Short: "Copy a conversation to another directory after stopping the source tree", Long: "Requires --confirm: source Agents, tasks and owned processes stop before the independent copy is published. Retains history, context, images and optional drafts. Returns acceptance; inspect operation inspect ID until completed. Retry with the original --request-id. The source becomes read-only.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !confirm || cwd == "" {
			return protocol.E("invalid_params", "copy requires --cwd PATH --confirm; the source Agent tree and its owned processes will stop")
		}
		p := protocol.CopyAgent{AgentID: args[0], Cwd: cwd, Confirm: true}
		if cmd.Flags().Changed("expected-revision") {
			p.ExpectedRevision = &revision
		}
		if drafts != "" {
			reader := cmd.InOrStdin()
			if drafts != "-" {
				f, err := os.Open(drafts)
				if err != nil {
					return protocol.E("invalid_params", "cannot open copy drafts file")
				}
				defer f.Close()
				reader = f
			}
			data, err := io.ReadAll(io.LimitReader(reader, protocol.MaxCopyDraftBytes+1))
			if err != nil {
				return protocol.E("invalid_params", "cannot read copy drafts")
			}
			if len(data) > protocol.MaxCopyDraftBytes || !utf8.Valid(data) || protocol.StrictDecode(data, &p.Drafts) != nil || p.Drafts == nil || len(p.Drafts) > protocol.MaxCopyDrafts {
				return protocol.E("invalid_params", "drafts must be a UTF-8 JSON array, up to 17 drafts and 6 MiB")
			}
		}
		caller, err := callerFromEnvironment()
		if err != nil {
			return err
		}
		p.Caller = caller
		return a.call(cmd, "agent.copy", true, p)
	}}
	command.Flags().StringVar(&cwd, "cwd", "", "Existing target directory, relative to the source Agent cwd")
	command.Flags().BoolVar(&confirm, "confirm", false, "Confirm stopping the source Agent tree and owned processes before copying")
	command.Flags().StringVar(&drafts, "draft-file", "", "Read a JSON array of retained input drafts from a file, or - for stdin")
	command.Flags().Int64Var(&revision, "expected-revision", 0, "Require the source revision returned by agent selection")
	agent.AddCommand(command)
	agent.AddCommand(&cobra.Command{Use: "copy-drafts ID", Short: "Read retained drafts after a session copy is published", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "agent.copy.drafts", false, map[string]string{"operation_id": args[0]})
	}})
}

func copyHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{{Name: "agent.copy", Usage: prefix + "--request-id ID agent copy AGENT --cwd PATH --confirm", Guidance: "Use when the user requests moving the conversation to another directory by copying. Stops the source Agent tree and its owned processes before publishing an independent copy. Original history stays read-only; completed effects are retained. Acceptance is not publication: inspect operation inspect ID until completed, then use its agent_id. Reuse the original request ID after uncertain acceptance. Optional --draft-file retains local input; copy-drafts ID reads it after publication."}}
}
