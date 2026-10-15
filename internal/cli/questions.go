package cli

import (
	"strings"

	"github.com/spf13/cobra"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/runtime"
)

func (a *app) questionCommands(root *cobra.Command) {
	cmd := &cobra.Command{Use: "question", Short: "Ask and answer durable questions for a running task"}
	var task, askFile string
	ask := &cobra.Command{Use: "ask AGENT [TEXT]", Short: "Save a question (up to 8 KiB); core waits after tools finish", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		if task == "" {
			return protocol.E("invalid_params", "--task is required")
		}
		text, err := readInput(cmd, args[1:], askFile, true)
		if err != nil {
			return err
		}
		return a.call(cmd, "question.ask", true, protocol.AskQuestion{AgentID: args[0], TaskID: task, Prompt: text})
	}}
	ask.Flags().StringVar(&task, "task", "", "Running task ID")
	ask.Flags().StringVar(&askFile, "file", "", "Read question from file, or - for stdin")
	cmd.AddCommand(ask)
	var answerFile string
	var images []string
	answer := &cobra.Command{Use: "answer AGENT QUESTION [TEXT]", Short: "Save an answer (up to 1 MiB) and optional image references", Args: cobra.RangeArgs(2, 3), RunE: func(cmd *cobra.Command, args []string) error {
		text, err := readInput(cmd, args[2:], answerFile, len(images) == 0)
		if err != nil {
			return err
		}
		return a.call(cmd, "question.answer", true, protocol.AnswerQuestion{AgentID: args[0], ID: args[1], Answer: text, Attachments: images})
	}}
	answer.Flags().StringVar(&answerFile, "file", "", "Read answer from file, or - for stdin")
	answer.Flags().StringArrayVar(&images, "image", nil, "Imported image ID (repeatable)")
	cmd.AddCommand(answer)
	cmd.AddCommand(&cobra.Command{Use: "inspect AGENT QUESTION", Short: "Read the question, answer, image references and delivery cursor", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "question.inspect", false, protocol.QuestionTarget{AgentID: args[0], ID: args[1]})
	}})
	var after, state string
	var limit int
	list := &cobra.Command{Use: "list AGENT", Short: "List question metadata without expanding answers", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.call(cmd, "question.list", false, protocol.QuestionList{AgentID: args[0], State: state, After: after, Limit: limit})
	}}
	list.Flags().StringVar(&state, "state", "pending", "pending, answered, cancelled or all")
	list.Flags().StringVar(&after, "after", "", "Last question ID from the previous page")
	list.Flags().IntVar(&limit, "limit", 20, "Maximum questions (1–100)")
	cmd.AddCommand(list)
	root.AddCommand(cmd)
}

func quoteCommandPart(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// These hints describe registered commands in this executable. The runtime
// includes them only when command execution is also available.
func questionHints(executable, profile string) []runtime.CommandHint {
	prefix := quoteCommandPart(executable) + " --profile " + quoteCommandPart(profile) + " "
	return []runtime.CommandHint{
		{Name: "question.ask", Usage: prefix + "--request-id ID question ask AGENT --task TASK --file FILE", Guidance: "Ask when missing user information is needed. Use the Agent and Task IDs below; FILE or stdin (-) holds up to 8 KiB. At most 8 questions may await answers. Core waits after tool results, then adds the user's answers and images to context. Do not answer on the user's behalf."},
		{Name: "question.inspect", Usage: prefix + "question inspect AGENT QUESTION", Guidance: "Read a saved question and its answer. An accepted ask is not an answer; preserve question_id and the original request ID on retries."},
	}
}
