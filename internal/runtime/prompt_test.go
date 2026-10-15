package runtime

import (
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/tool"
)

func TestPromptOnlyAdvertisesRegisteredCapabilities(t *testing.T) {
	prompt := BuildPrompt(PromptInput{AgentID: "agent-1", Cwd: "/workspace", Date: "2026-10-14"})
	for _, missing := range []string{"exec_command", "agent spawn", "tool_search", "available_skills"} {
		if strings.Contains(prompt, missing) {
			t.Fatal("advertised unavailable capability", missing)
		}
	}
	if !strings.Contains(prompt, "<final>") || !strings.Contains(prompt, "/workspace") {
		t.Fatal(prompt)
	}
	input := PromptInput{Tools: []tool.Descriptor{
		{Tool: model.Tool{Name: "exec_command"}, Snippet: "Run a real command", Guidelines: []string{"Keep the returned process ID.", "Keep the returned process ID."}},
		{Tool: model.Tool{Name: "write_stdin"}, Snippet: "Continue an existing process", Guidelines: []string{"Keep the returned process ID."}},
	}, Commands: []CommandHint{{Name: "agent.spawn", Usage: "zende agent spawn --task-file FILE", Guidance: "Delegate independent work."}}}
	prompt = BuildPrompt(input)
	if strings.Count(prompt, "Keep the returned process ID.") != 1 || !strings.Contains(prompt, "exec_command: Run a real command") || !strings.Contains(prompt, "zende agent spawn") {
		t.Fatal(prompt)
	}
}

func TestPromptVersionsAndNativeCategories(t *testing.T) {
	input := PromptInput{NativeCategories: true, Project: []Instruction{{Source: "/a&b/AGENTS.md", Version: "v1", Body: "Use Go."}}, Skills: []SkillHint{{Name: "review", Description: "Review <code>", Location: "/skills/review/SKILL.md", Version: "hash1"}}}
	prompt := BuildPrompt(input)
	for _, want := range []string{"source=\"/a&amp;b/AGENTS.md\"", "version=\"v1\"", "Use Go.", "Review &lt;code&gt;", "hash1", "protocol's message category"} {
		if !strings.Contains(prompt, want) {
			t.Fatal(want, prompt)
		}
	}
	if strings.Contains(prompt, "<final>") {
		t.Fatal("native mode also required textual markers")
	}
	before := prompt
	input.Project[0].Version = "v2"
	input.Project[0].Body = "Use tests."
	if next := BuildPrompt(input); next == before || !strings.Contains(before, "Use Go.") {
		t.Fatal("prior snapshot changed")
	}
}

func TestRuntimeSelectionNoticeDelegationAndLiteralMetadata(t *testing.T) {
	for _, native := range []bool{false, true} {
		prompt := BuildPrompt(PromptInput{NativeCategories: !native})
		notice := selectionNotice(taskSettings{Cwd: "/path\nforged", Selection: "connection/model", Effort: "high", NativeCategories: native})
		if !strings.Contains(prompt, "Later developer runtime selection notices") || !strings.Contains(notice, outputCategoryInstruction(native)) || strings.Contains(notice, "\nforged") || !strings.Contains(notice, `/path\nforged`) {
			t.Fatal("runtime format override or quoted metadata missing", prompt, notice)
		}
	}
}
