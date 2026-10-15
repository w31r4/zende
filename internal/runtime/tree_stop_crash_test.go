package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

type freezeBeforeCrash struct{ process.Remote }

func (r freezeBeforeCrash) FreezeStop(ctx context.Context, id string, scope process.StopScope) (process.StopRecord, error) {
	value, err := r.Remote.FreezeStop(ctx, id, scope)
	if err == nil {
		<-ctx.Done()
		return value, ctx.Err()
	}
	return value, err
}

func TestTreeStopCrashHelper(t *testing.T) {
	root := os.Getenv("ZENDE_TEST_TREE_STOP_ROOT")
	if root == "" {
		return
	}
	ctx := context.Background()
	socket := filepath.Join(root, "r.sock")
	if os.Getenv("ZENDE_TEST_TREE_STOP_ROLE") == "runner" {
		m, err := process.NewManager(filepath.Join(root, "processes"), "/bin/sh", 8, 65536)
		if err != nil {
			t.Fatal(err)
		}
		server, err := protocol.Listen(socket, process.Service{Manager: m})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Serve(ctx); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := os.Getenv("ZENDE_TEST_TREE_STOP_DB")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s, resource.Effective{Config: resource.Defaults(filepath.Dir(path))})
	r := process.Remote{Client: protocol.Client{Socket: socket}}
	c.Runner = freezeBeforeCrash{r}
	c.Tools, err = tool.NewRegistry(tool.ProcessExecutors(r)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTreeStopSurvivesCoreAndRunnerSIGKILL(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("real tmux required")
	}
	ctx := context.Background()
	root := t.TempDir()
	remote := process.Remote{Client: protocol.Client{Socket: filepath.Join(root, "r.sock")}}
	start := func(role, db string) func() {
		t.Helper()
		log, err := os.CreateTemp(root, role+"-log-")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestTreeStopCrashHelper$")
		cmd.Env = append(os.Environ(), "ZENDE_TEST_TREE_STOP_ROOT="+root, "ZENDE_TEST_TREE_STOP_ROLE="+role, "ZENDE_TEST_TREE_STOP_DB="+db)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		dead := false
		stop := func() {
			if !dead {
				dead = true
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				_ = log.Close()
			}
		}
		t.Cleanup(stop)
		if role == "runner" {
			awaitWorkspace(t, "runner did not listen", func() bool {
				req, _ := protocol.NewRequest("runner.status", "", struct{}{})
				var status struct {
					PID int `json:"pid"`
				}
				return remote.Client.Call(ctx, req, &status) == nil && status.PID == cmd.Process.Pid
			})
		}
		return stop
	}
	stopRunner := start("runner", "")
	script := fixture.New()
	c := loopCore(t, script, tool.ProcessExecutors(remote)...)
	c.Runner = remote
	a, appendEvent, _ := savedWork(t, c, script)
	call := model.ToolCall{ID: "original-call", Name: "exec_command", Arguments: json.RawMessage(`{"command":"printf x >> marker; printf retained; sleep 60","wait_ms":0}`)}
	journalResponse(t, c, a, call)
	op := invocation{SchemaVersion: 2, ID: "original-process", RequestID: "request-original", Call: call, Version: "2"}
	appendEvent("tool.started", op)
	appendEvent("tool.dispatching", op)
	for _, pty := range []bool{false, true} {
		id, command, stream := op.ID, "printf x >> marker; printf retained; sleep 60", "stdout"
		if pty {
			id, command, stream = "background-terminal", "printf retained; sleep 60", "pty"
		}
		state, err := remote.Start(ctx, process.Spec{ID: id, AgentID: a.AgentID, TaskID: a.TaskID, Cwd: c.Config.Config.Cwd, Command: command, PTY: pty})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			group, ended, err := process.ObserveGroup(process.Group{PID: state.PID, Identity: state.Identity})
			if err == nil && !ended {
				_ = process.KillGroup(group)
			}
			if state.Terminal != nil {
				_ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").Run()
			}
		})
		awaitWorkspace(t, "command did not produce output", func() bool {
			out, err := remote.Read(ctx, id, stream, 0, 65536)
			return err == nil && strings.Contains(out.Text, "retained")
		})
	}
	// Lose the runner before recovery can obtain a known pipe exit status.
	stopRunner()
	stopReplacement := start("runner", "")
	req := requestTreeStop(t, c, a.AgentID, "crash-tree")
	stopCore := start("core", filepath.Join(c.Config.Config.Cwd, "state.db"))
	awaitWorkspace(t, "core did not freeze runner", func() bool {
		r, err := remote.InspectStop(ctx, req.OperationID)
		return err == nil && r.State == "frozen"
	})
	stopCore()
	stopReplacement() // Crash again after the scope receipt was committed.
	start("runner", "")
	c = reopenCore(t, c)
	c.Runner = remote
	c.Models = nil
	runCore(t, c)
	awaitTreeStop(t, c, req.OperationID)
	waitTask(t, c, a, "cancelled")
	if got, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker")); err != nil || string(got) != "x" {
		t.Fatal("command replayed", string(got), err)
	}
	state, err := remote.Inspect(ctx, op.ID)
	if err != nil || state.ExitCode != nil || state.Status != "lost" && state.Status != "unknown" {
		t.Fatal("invented pipe outcome", state, err)
	}
	page, err := c.Store.Events(ctx, protocol.Cursor{AgentID: a.AgentID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, event := range page.Events {
		if event.Kind != "tool.completed" {
			continue
		}
		var outcome toolOutcome
		if err := json.Unmarshal(event.Payload, &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome.Invocation.ID != op.ID {
			continue
		}
		verified++
		var result tool.ProcessResult
		if len(outcome.Result.Content) != 1 || json.Unmarshal([]byte(outcome.Result.Content[0].Text), &result) != nil {
			t.Fatal(outcome)
		}
		if outcome.Error == nil || outcome.Error.Data.Kind != "task_cancelled" || result.Stop == nil || result.Stop.OperationID != req.OperationID || result.Stop.State != "stopped" || result.ExitCode != nil || len(result.Output) == 0 || !strings.Contains(result.Output[0].Text, "retained") {
			t.Fatal(outcome, result)
		}
	}
	if verified != 1 {
		t.Fatal("missing original call cancellation", verified)
	}
}
