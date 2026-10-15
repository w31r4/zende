package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestQuestionCLIUsesDurableCoreAndStableRequests(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-question-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	a := &app{profile: "questions"}
	paths, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	server, err := protocol.Listen(paths.Socket, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	run := func(input string, args ...string) []byte {
		t.Helper()
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "questions"}, args...), strings.NewReader(input), &out, &diagnostics, BuildInfo{})
		if code != 0 || diagnostics.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostics.String())
		}
		return out.Bytes()
	}
	var created protocol.Accepted
	if err := json.Unmarshal(run("", "agent", "create", "initial", "--cwd", dir), &created); err != nil {
		t.Fatal(err)
	}
	work, err := s.ClaimNext(ctx, created.AgentID)
	if err != nil || work == nil {
		t.Fatal(work, err)
	}
	args := []string{"--request-id", "ask", "question", "ask", created.AgentID, "--task", work.Task.ID, "--file", "-"}
	first := run("中文问题\n第二行", args...)
	if !bytes.Equal(first, run("中文问题\n第二行", args...)) {
		t.Fatal("ask retry changed receipt")
	}
	var asked protocol.Accepted
	if err := json.Unmarshal(first, &asked); err != nil || asked.QuestionID == "" {
		t.Fatal(asked, err)
	}
	var page protocol.QuestionPage
	if err := json.Unmarshal(run("", "question", "list", created.AgentID, "--limit", "1"), &page); err != nil || len(page.Items) != 1 || page.Items[0].Prompt != "中文问题\n第二行" {
		t.Fatal(page, err)
	}
	answerFile := filepath.Join(dir, "中文 answer.txt")
	if err := os.WriteFile(answerFile, []byte("中文回答\n第二行"), 0600); err != nil {
		t.Fatal(err)
	}
	args = []string{"--request-id", "answer", "question", "answer", created.AgentID, asked.QuestionID, "--file", answerFile}
	answered := run("", args...)
	if !bytes.Equal(answered, run("", args...)) {
		t.Fatal("answer retry changed receipt")
	}
	var q protocol.Question
	if err := json.Unmarshal(run("", "question", "inspect", created.AgentID, asked.QuestionID), &q); err != nil || q.Answer != "中文回答\n第二行" || q.DeliveredSeq != 0 {
		t.Fatal(q, err)
	}
	if ready, err := s.QuestionBarrier(ctx, created.AgentID, work.Task.ID); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if err := json.Unmarshal(run("", "question", "list", created.AgentID, "--state", "answered"), &page); err != nil || len(page.Items) != 1 || page.Items[0].DeliveredSeq == 0 {
		t.Fatal(page, err)
	}
	if bytes.Contains(run("", "question", "list", created.AgentID, "--state", "all"), []byte("中文回答")) {
		t.Fatal("list expanded answer")
	}
	for _, args := range [][]string{{"question", "--help"}, {"question", "ask", "--help"}, {"question", "answer", "--help"}, {"question", "inspect", "--help"}, {"question", "list", "--help"}} {
		if !bytes.Contains(run("", args...), []byte("Usage:")) {
			t.Fatal("missing question help", args)
		}
	}
	for _, args := range [][]string{{"question", "ask", created.AgentID, "missing task"}, {"question", "ask", created.AgentID, "--task", work.Task.ID}, {"question", "answer", created.AgentID, asked.QuestionID}, {"question", "list", created.AgentID, "--limit", "101"}} {
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--profile", "questions"}, args...), nil, &out, &diagnostics, BuildInfo{})
		if code != 2 || out.Len() != 0 || diagnostics.Len() == 0 {
			t.Fatal("invalid CLI did not separate diagnostics", args, code, out.String(), diagnostics.String())
		}
	}
}

func TestQuestionPromptHintsMatchRegisteredCLIAndQuotePaths(t *testing.T) {
	root := (&app{}).command()
	hints := questionHints("/a'b/with space/zende", "profile $literal")
	for _, hint := range hints {
		parts := strings.Split(hint.Name, ".")
		cmd, remaining, err := root.Find(parts)
		if err != nil || len(remaining) != 0 || cmd.Name() != parts[1] || cmd.RunE == nil {
			t.Fatal("hint advertises absent command", hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "--profile 'profile $literal'") {
			t.Fatal("missing profile", hint.Usage)
		}
	}
	for _, value := range []string{"space in path", "a'b", "$HOME; false", "中文\nname"} {
		out, err := exec.Command("/bin/sh", "-c", "printf '%s' "+quoteCommandPart(value)).Output()
		if err != nil || !reflect.DeepEqual(out, []byte(value)) {
			t.Fatal("CLI hint shell quoting", err, string(out), value)
		}
	}
}
