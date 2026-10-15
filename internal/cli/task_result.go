package cli

import (
	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
)

func (a *app) resultCommands(agent *cobra.Command) {
	var p protocol.WaitTask
	wait := &cobra.Command{Use: "wait AGENT --task TASK", Short: "Wait for one task; timeout never cancels it", Long: "Returns terminal state and a readable result reference. Inside Agent commands, core attaches the full result to the observing tool context when it fits the tool budget; stdout alone does not consume mailbox delivery. Large results stay queued. Use agent result to read UTF-8 pages.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p.AgentID = args[0]
		var err error
		p.Caller, err = callerFromEnvironment()
		if err != nil {
			return err
		}
		if p.TaskID == "" || p.WaitMS < 0 || p.WaitMS > protocol.MaxAgentWaitMS {
			return protocol.E("invalid_params", "--task is required; --wait-ms must be 0..30000")
		}
		return a.call(cmd, "agent.wait", false, p)
	}}
	wait.Flags().StringVar(&p.TaskID, "task", "", "Exact task identity, not whichever task runs last")
	wait.Flags().IntVar(&p.WaitMS, "wait-ms", protocol.DefaultAgentWaitMS, "Maximum observation time (0–30000); does not stop the task")
	agent.AddCommand(wait)
	var read protocol.ReadTaskResult
	result := &cobra.Command{Use: "result AGENT --task TASK", Short: "Read an immutable completed result by UTF-8 byte cursor", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		read.AgentID = args[0]
		if read.TaskID == "" || read.Offset < 0 || read.Limit < 1 || read.Limit > protocol.MaxResultPageBytes {
			return protocol.E("invalid_params", "--task is required; --offset must be nonnegative and --limit must be 1..65536")
		}
		return a.call(cmd, "agent.result", false, read)
	}}
	result.Flags().StringVar(&read.TaskID, "task", "", "Completed task identity")
	result.Flags().IntVar(&read.Offset, "offset", 0, "Original UTF-8 byte cursor from next")
	result.Flags().IntVar(&read.Limit, "limit", protocol.MaxResultPageBytes, "Maximum UTF-8 bytes per page (1–65536)")
	agent.AddCommand(result)
}
