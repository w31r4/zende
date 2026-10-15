package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

func TestSpawnCLIAttributionIdempotencyAndReuse(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zende-spawn-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv(protocol.CallerEnvironment, "")
	paths, err := (&app{profile: "spawn"}).paths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core := runtime.New(s, resource.Effective{Config: resource.Defaults(dir)})
	server, err := protocol.Listen(paths.Socket, core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		var out, diagnostic bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "spawn"}, args...), strings.NewReader(input), &out, &diagnostic, BuildInfo{}); code != 0 || diagnostic.Len() != 0 || bytes.Contains(out.Bytes(), []byte{27}) {
			t.Fatal(args, code, diagnostic.String(), out.String())
		}
		return out.Bytes()
	}
	var parent protocol.Accepted
	if err := json.Unmarshal(run("", "agent", "create", "--cwd", dir, "parent-task"), &parent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNext(ctx, parent.AgentID); err != nil {
		t.Fatal(err)
	}
	caller := protocol.Caller{AgentID: parent.AgentID, TaskID: parent.TaskID, OperationID: "host-operation"}
	if err := s.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", map[string]any{"operation_id": caller.OperationID, "request_id": "request", "call": model.ToolCall{ID: "call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(caller)
	t.Setenv(protocol.CallerEnvironment, string(raw))
	input := "独立任务\nsecond line"
	acceptedBytes := run(input, "agent", "spawn", "--task-file", "-")
	var child protocol.Accepted
	if err := json.Unmarshal(acceptedBytes, &child); err != nil || child.Spawn == nil || child.Spawn.Caller == nil || *child.Spawn.Caller != caller || child.Spawn.Messages != 1 || child.Spawn.CallID != "call" || child.TaskID == "" {
		t.Fatal(child, err)
	}
	if repeated := run(input, "agent", "spawn", "--task-file", "-"); !bytes.Equal(repeated, acceptedBytes) {
		t.Fatal("default operation not stable", string(repeated))
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(run("", "agent", "inspect", child.AgentID), &snap); err != nil || snap.Agent.Cwd != dir || snap.Agent.ParentID != parent.AgentID || snap.Agent.State != "idle" || snap.Tasks[0].State != "accepted" || snap.Tasks[0].Input != input {
		t.Fatal(snap, err)
	}
	// Core is serving IPC but no scheduler is running. Spawn has already returned
	// its durable receipt and did not need a model, terminal, or TUI.
	if _, err := s.ClaimNext(ctx, child.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, child.AgentID, child.TaskID, "completed", "done", nil); err != nil {
		t.Fatal(err)
	}
	follow := run("continue", "agent", "submit", child.AgentID, "--file", "-")
	if repeated := run("continue", "agent", "submit", child.AgentID, "--file", "-"); !bytes.Equal(repeated, follow) {
		t.Fatal("submission retry duplicated task")
	}
	var continued protocol.Accepted
	if json.Unmarshal(follow, &continued) != nil || continued.AgentID != child.AgentID || continued.TaskID == child.TaskID {
		t.Fatal(string(follow))
	}
	var sent protocol.Accepted
	message := run("peer message", "agent", "send", child.AgentID, "--file", "-", "--delivery", "steer")
	if err := json.Unmarshal(message, &sent); err != nil || sent.MessageID == "" || sent.TaskID != "" {
		t.Fatal(string(message), err)
	}
	if repeated := run("peer message", "agent", "send", child.AgentID, "--file", "-", "--delivery", "steer"); !bytes.Equal(message, repeated) {
		t.Fatal("send retry duplicated message")
	}
	var inbox protocol.MailboxPage
	if err := json.Unmarshal(run("", "mailbox", "list", "--direction", "outbox"), &inbox); err != nil || len(inbox.Items) != 1 || inbox.Items[0].ID != sent.MessageID {
		t.Fatal(inbox, err)
	}
	var mail protocol.Mail
	if err := json.Unmarshal(run("", "mailbox", "inspect", sent.MessageID), &mail); err != nil || mail.Source == nil || mail.Source.Caller != caller || mail.Input != "peer message" || mail.State != "accepted" {
		t.Fatal(mail, err)
	}
	history, err := s.Events(ctx, protocol.Cursor{AgentID: child.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range history.Events {
		if event.Kind == "task.submitted" {
			found++
			var submitted struct {
				Source protocol.CallSource `json:"source"`
			}
			if err := json.Unmarshal(event.Payload, &submitted); err != nil || submitted.Source.Caller != caller || submitted.Source.CallID != "call" || event.TaskID != continued.TaskID {
				t.Fatal(event, err)
			}
		}
	}
	if found != 1 {
		t.Fatal("submission provenance missing/duplicated", found)
	}
	// A new exec_command has different provenance. Explicitly retrying the
	// original operation still returns the original receipt, not a conflict.
	nextCaller := caller
	nextCaller.OperationID = "retry-host-operation"
	if err := s.AppendTaskEvent(ctx, parent.AgentID, parent.TaskID, "tool.dispatching", map[string]any{"operation_id": nextCaller.OperationID, "request_id": "retry-request", "call": model.ToolCall{ID: "retry-call", Name: "exec_command"}}); err != nil {
		t.Fatal(err)
	}
	nextRaw, _ := json.Marshal(nextCaller)
	t.Setenv(protocol.CallerEnvironment, string(nextRaw))
	if repeated := run("peer message", "--request-id", sent.OperationID, "agent", "send", child.AgentID, "--file", "-", "--delivery", "steer"); !bytes.Equal(message, repeated) {
		t.Fatal("retry caller changed message")
	}
	if retried := run(input, "--request-id", child.OperationID, "agent", "spawn", "--task-file", "-"); !bytes.Equal(retried, acceptedBytes) {
		t.Fatal("new caller changed accepted spawn", string(retried))
	}
	if retried := run("continue", "--request-id", continued.OperationID, "agent", "submit", child.AgentID, "--file", "-"); !bytes.Equal(retried, follow) {
		t.Fatal("new caller changed accepted submit", string(retried))
	}
	t.Setenv(protocol.CallerEnvironment, string(raw))
	agents, _ := s.ListAgents(ctx)
	if len(agents) != 2 {
		t.Fatal("reuse created another Agent", agents)
	}
	// Explicit request IDs can create intentionally identical children.
	var other protocol.Accepted
	if json.Unmarshal(run(input, "--request-id", "different-child", "agent", "spawn", "--task-file", "-"), &other) != nil || other.AgentID == child.AgentID {
		t.Fatal(other)
	}
	// Future parent settings never retarget the child or alter a retry receipt.
	newCwd := t.TempDir()
	run("", "agent", "select", parent.AgentID, "--cwd", newCwd)
	if repeated := run(input, "agent", "spawn", "--task-file", "-"); !bytes.Equal(repeated, acceptedBytes) {
		t.Fatal("retry used new parent state")
	}
	saved, _ := s.Selection(ctx, child.AgentID)
	if saved.Choice.Cwd != dir {
		t.Fatal("child followed parent cwd change")
	}
	for _, args := range [][]string{
		{"agent", "spawn", "--parent", "another", "bad"},
		{"agent", "spawn", "--context", "last:0", "invalid count"},
		{"agent", "spawn", "--workspace", "invalid", "bad"},
		{"agent", "spawn", "--workspace", "worktree", "missing path"},
		{"agent", "spawn", "--worktree-path", "unused", "missing mode"},
		{"agent", "spawn", "--cwd", "does-not-exist", "bad"},
		{"--request-id", child.OperationID, "agent", "spawn", "conflicting"},
		{"agent", "spawn", "--file", "-", "--task-file", "-"},
		{"agent", "spawn", "--file", "-", "--background-file", "-"},
		{"agent", "spawn", "--background", "x", "--background-file", "-", "task"},
		{"agent", "spawn", "--context-budget", "0", "task"},
		{"agent", "spawn", "--context-budget", "-1", "task"},
		{"agent", "spawn", "--background-history", "bad", "task"},
		{"agent", "spawn", "--background-history", "agent:2:1", "task"},
	} {
		var out, diagnostic bytes.Buffer
		if code := Run(ctx, append([]string{"--profile", "spawn"}, args...), strings.NewReader("x"), &out, &diagnostic, BuildInfo{}); code == 0 || out.Len() != 0 {
			t.Fatal(args, code, out.String())
		}
	}
	t.Setenv(protocol.CallerEnvironment, "")
	var human protocol.Accepted
	if json.Unmarshal(run("", "--request-id", "human", "agent", "spawn", "--parent", parent.AgentID, "--context", "none", "human-task"), &human) != nil || human.Spawn.Caller != nil || human.Spawn.Messages != 0 {
		t.Fatal(human)
	}
	var limited protocol.Accepted
	if err := json.Unmarshal(run("", "agent", "spawn", "--parent", parent.AgentID, "--context", "last:1", "--context-budget", "1000", "limited"), &limited); err != nil || limited.Spawn.Context != "last:1" || limited.Spawn.Budget != 1000 || limited.Spawn.Messages != 1 {
		t.Fatal(limited, err)
	}
	var withFile protocol.Accepted
	if err := json.Unmarshal(run("补充背景\nfrom stdin", "agent", "spawn", "--parent", parent.AgentID, "--context", "none", "--background-file", "-", "file-background"), &withFile); err != nil || withFile.Spawn.BackgroundMessages != 1 {
		t.Fatal(withFile, err)
	}
	fileMessages, _, err := s.ModelContext(ctx, withFile.AgentID)
	if err != nil || !strings.Contains(fileMessages[0].Content[0].Text, "补充背景\nfrom stdin") {
		t.Fatal(fileMessages, err)
	}
	var referenced protocol.Accepted
	rangeArg := parent.AgentID + ":" + strconv.FormatInt(child.Spawn.First, 10) + ":" + strconv.FormatInt(child.Spawn.Last, 10)
	if err := json.Unmarshal(run("", "agent", "spawn", "--parent", parent.AgentID, "--context", "none", "--background-history", rangeArg, "--background", "specific evidence", "raw-background"), &referenced); err != nil || referenced.Spawn.BackgroundMessages != 2 || len(referenced.Spawn.BackgroundHistory) != 1 {
		t.Fatal(referenced, err)
	}
	var budgetOnly protocol.Accepted
	if err := json.Unmarshal(run("", "agent", "spawn", "--parent", parent.AgentID, "--context-budget", "1000", "budget-only"), &budgetOnly); err != nil || budgetOnly.Spawn.Context != "budget" || budgetOnly.Spawn.Messages != 1 {
		t.Fatal(budgetOnly, err)
	}
	for _, env := range []string{"", "bad-json", "null", `{"agent_id":"wrong"}`, `{"agent_id":"x","task_id":"y","operation_id":"z"}`} {
		t.Setenv(protocol.CallerEnvironment, env)
		for _, command := range []string{"spawn", "submit"} {
			if command == "submit" && env == "" {
				continue
			}
			args := []string{"--profile", "spawn", "agent", command}
			if command == "submit" {
				args = append(args, child.AgentID)
			}
			args = append(args, "bad")
			var out, diagnostic bytes.Buffer
			if code := Run(ctx, args, nil, &out, &diagnostic, BuildInfo{}); code == 0 || out.Len() != 0 {
				t.Fatal("invalid binding accepted", env, command)
			}
		}
	}
}

func TestCollaborationHintsOnlyRegisteredCommands(t *testing.T) {
	root := (&app{}).command()
	for _, hint := range collaborationHints("/path with space/zende", "profile") {
		args := strings.Split(hint.Name, ".")
		command, rest, err := root.Find(args)
		if err != nil || len(rest) != 0 || command.Name() != args[len(args)-1] {
			t.Fatal(hint.Name, err)
		}
		if !strings.Contains(hint.Usage, "'/path with space/zende' --profile 'profile'") {
			t.Fatal("unquoted CLI hint")
		}
		if hint.Name == "agent.spawn" {
			for _, text := range []string{"--workspace worktree --worktree-path PATH", "detached HEAD", "uncommitted", "original --request-id", "no automatic merge or removal"} {
				if !strings.Contains(hint.Guidance, text) {
					t.Fatal("workspace hint missing", text)
				}
			}
		}
	}
}
