package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
)

func (a *app) attachmentCommands(root *cobra.Command) {
	cmd := &cobra.Command{Use: "attachment", Short: "Import and inspect retained image bytes"}
	cmd.AddCommand(&cobra.Command{Use: "import AGENT PATH", Short: "Save PNG/JPEG bytes; relative paths use the Agent cwd", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "attachment.import", true, protocol.ImportAttachment{AgentID: args[0], Path: args[1]})
	}})
	for _, action := range []string{"inspect", "read"} {
		short := "Inspect saved image metadata"
		if action == "read" {
			short = "Read saved bytes as base64 JSON after integrity verification"
		}
		cmd.AddCommand(&cobra.Command{Use: action + " AGENT IMAGE", Short: short, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			return a.call(cmd, "attachment."+action, false, protocol.AttachmentTarget{AgentID: args[0], ID: args[1]})
		}})
	}
	var after string
	var limit int
	list := &cobra.Command{Use: "list AGENT", Short: "List image metadata in bounded pages", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "attachment.list", false, protocol.AttachmentList{AgentID: args[0], After: after, Limit: limit})
	}}
	list.Flags().StringVar(&after, "after", "", "Last image ID from the previous page")
	list.Flags().IntVar(&limit, "limit", 20, "Maximum images (1–100)")
	cmd.AddCommand(list)
	root.AddCommand(cmd)
}
