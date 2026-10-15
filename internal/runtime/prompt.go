package runtime

import (
	"fmt"
	"html"
	"strings"

	"zende.local/harness/internal/tool"
)

// PromptInput contains only registered capabilities and public context metadata.
// The caller persists the assembled prompt and its source versions before use.
type PromptInput struct {
	AgentID          string
	TaskID           string
	Cwd              string
	Date             string
	Tools            []tool.Descriptor
	Commands         []CommandHint
	Project          []Instruction
	Skills           []SkillHint
	NativeCategories bool
}

type CommandHint struct {
	Name     string
	Usage    string
	Guidance string
}

type Instruction struct{ Source, Version, Body string }
type SkillHint struct{ Name, Description, Location, Version string }

// BuildPrompt follows Pi's capability-conditioned snippets and deduplicated
// guidelines, while keeping Zende's terminal-first tool contracts.
func BuildPrompt(input PromptInput) string {
	var b strings.Builder
	b.WriteString("You are Zende, a coding agent in a persistent local session. Inspect the environment, carry out the user's requested work, and verify the result. Preserve unrelated work. Report results and unresolved errors concisely, with useful file paths.\n")
	if len(input.Tools) > 0 {
		b.WriteString("\nAvailable tools:\n")
		for _, t := range input.Tools {
			if t.Snippet != "" {
				fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Snippet)
			}
		}
	}
	rules := []string{}
	seen := map[string]bool{}
	for _, t := range input.Tools {
		for _, rule := range t.Guidelines {
			rule = strings.TrimSpace(rule)
			if rule != "" && !seen[rule] {
				seen[rule] = true
				rules = append(rules, rule)
			}
		}
	}
	if len(rules) > 0 {
		b.WriteString("\nTool guidelines:\n")
		for _, rule := range rules {
			fmt.Fprintf(&b, "- %s\n", rule)
		}
	}
	if len(input.Commands) > 0 {
		b.WriteString("\nHost CLI (available through command execution):\n")
		for _, command := range input.Commands {
			fmt.Fprintf(&b, "- %s: %s\n", command.Usage, command.Guidance)
		}
		b.WriteString("Use --help for less common parameters. Keep the same request ID when retrying an accepted operation.\n")
	}
	if input.NativeCategories {
		b.WriteString("\nClassify public progress as commentary and the finished answer as final using the protocol's message category. These are visible messages, not private reasoning.\n")
	} else {
		b.WriteString("\nWrap public progress in <commentary>...</commentary> and the finished answer in <final>...</final>. Put each opening and closing tag on its own line. Blocks cannot nest. Emit at most one final, with no visible text or new tool calls after it. Keep literal tag examples in fenced code or escape them. Do not expose private reasoning.\n")
	}
	for _, instruction := range input.Project {
		fmt.Fprintf(&b, "\n<project_instructions source=%q version=%q>\n%s\n</project_instructions>\n", html.EscapeString(instruction.Source), html.EscapeString(instruction.Version), instruction.Body)
	}
	if len(input.Skills) > 0 {
		b.WriteString("\nRead a matching skill's complete instructions before using it. Resolve relative resources against its directory. Execute scripts through the process tools.\n<available_skills>\n")
		for _, skill := range input.Skills {
			fmt.Fprintf(&b, "<skill name=%q location=%q version=%q>%s</skill>\n", html.EscapeString(skill.Name), html.EscapeString(skill.Location), html.EscapeString(skill.Version), html.EscapeString(skill.Description))
		}
		b.WriteString("</available_skills>\n")
	}
	fmt.Fprintf(&b, "\nAgent: %s\nWorking directory: %s\nDate: %s\n", input.AgentID, input.Cwd, input.Date)
	if input.TaskID != "" {
		fmt.Fprintf(&b, "Task: %s\n", input.TaskID)
	}
	return b.String()
}
