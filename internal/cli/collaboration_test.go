package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		{"agent", "spawn", "--context", "last:3", "not implemented"},
		{"agent", "spawn", "--cwd", "does-not-exist", "bad"},
		{"--request-id", child.OperationID, "agent", "spawn", "conflicting"},
		{"agent", "spawn", "--file", "-", "--task-file", "-"},
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
	}
}
