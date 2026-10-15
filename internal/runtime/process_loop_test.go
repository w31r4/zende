package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/process"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
	"zende.local/harness/internal/tool"
)

func TestMain(m *testing.M) {
	if handled, code := process.RunInternalPTY(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func remoteRunner(t *testing.T) (*process.Manager, process.Remote, string) {
	t.Helper()
	root := t.TempDir()
	m, err := process.NewManager(filepath.Join(root, "processes"), "/bin/sh", 8, 65536)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "runner.sock")
	server, err := protocol.Listen(socket, process.Service{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return m, process.Remote{Client: protocol.Client{Socket: socket}}, filepath.Join(root, "processes")
}

type checkedIntent struct {
	tool.Executor
	store *store.Store
	count *atomic.Int32
}

func (e checkedIntent) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	page, err := e.store.Events(ctx, protocol.Cursor{AgentID: call.AgentID, Limit: 1000})
	if err != nil {
		return tool.Result{}, err
	}
	found := false
	for _, event := range page.Events {
		if event.Kind == "tool.started" {
			var intent invocation
			if json.Unmarshal(event.Payload, &intent) == nil && intent.ID == call.ID {
				found = true
			}
		}
	}
	if !found {
		return tool.Result{}, fmt.Errorf("no durable intent before dispatch")
	}
	e.count.Add(1)
	return e.Executor.Execute(ctx, call)
}

func TestLoopExecAndContinueRealRunner(t *testing.T) {
	for _, pty := range []bool{false, true} {
		t.Run(fmt.Sprint("pty-", pty), func(t *testing.T) {
			if pty {
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("real tmux required")
				}
			}
			manager, remote, root := remoteRunner(t)
			var requests, dispatches atomic.Int32
			var original string
			adapter := adapterFunc(func(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
				var events []model.Delta
				switch requests.Add(1) {
				case 1:
					if len(r.Tools) != 2 || !strings.Contains(r.System, "exec_command") || !strings.Contains(r.System, "write_stdin") {
						return fmt.Errorf("registered tools missing from prompt")
					}
					command := "printf x >> marker; printf ready; read value; printf 'got:%s' \"$value\""
					if pty {
						command = "stty -echo; " + command
					}
					args, _ := json.Marshal(map[string]any{"command": command, "pty": pty, "wait_ms": 100})
					middle := len(args) / 2
					events = fixture.Call("provider-exec", "exec_command", string(args[:middle]), string(args[middle:]))
				case 2:
					last := r.Messages[len(r.Messages)-1]
					var output tool.ProcessResult
					if last.Role != "tool" || json.Unmarshal([]byte(last.Content[0].Text), &output) != nil || output.Status != "running" || output.ID == "" {
						return fmt.Errorf("process identity missing from tool result")
					}
					original = output.ID
					offsets := map[string]int64{}
					for _, page := range output.Output {
						offsets[page.Stream] = page.Next
					}
					args, _ := json.Marshal(map[string]any{"process_id": original, "chars": "hello\n", "offsets": offsets, "wait_ms": 3000})
					events = fixture.Call("provider-input", "write_stdin", string(args))
				case 3:
					last := r.Messages[len(r.Messages)-1]
					var output tool.ProcessResult
					if json.Unmarshal([]byte(last.Content[0].Text), &output) != nil || output.ID != original || output.Status != "exited" || output.Written == nil || *output.Written != 6 || !strings.Contains(output.Output[0].Text, "got:hello") {
						return fmt.Errorf("process continuation failed")
					}
					events = fixture.Answer("final", "real process finished")
				default:
					return fmt.Errorf("unexpected model request")
				}
				for _, event := range events {
					if err := emit(event); err != nil {
						return err
					}
				}
				return nil
			})
			c := loopCore(t, adapter)
			executors := []tool.Executor{}
			for _, executor := range tool.ProcessExecutors(remote) {
				executors = append(executors, checkedIntent{executor, c.Store, &dispatches})
			}
			var err error
			c.Tools, err = tool.NewRegistry(executors...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Resolve only identities owned by this test's runner directory.
				dirs, _ := os.ReadDir(root)
				for _, dir := range dirs {
					state, err := manager.Inspect(context.Background(), dir.Name())
					if err != nil {
						continue
					}
					if state.Status == "running" || state.Status == "unknown" {
						_, _ = manager.Signal(context.Background(), protocol.NewID("cleanup"), process.SignalParams{ID: state.ID, Signal: "KILL"})
						_, _ = manager.Wait(context.Background(), state.ID, time.Second)
					}
					if state.Terminal != nil {
						_ = exec.Command("tmux", "-S", state.Terminal.Socket, "kill-server").Run()
					}
				}
			})
			a := createWork(t, c, "run and continue a command")
			runCore(t, c)
			waitTask(t, c, a, "completed")
			if requests.Load() != 3 || dispatches.Load() != 2 {
				t.Fatal(requests.Load(), dispatches.Load())
			}
			state, err := manager.Inspect(context.Background(), original)
			if err != nil || state.AgentID != a.AgentID || state.TaskID != a.TaskID {
				t.Fatal(state, err)
			}
			marker, err := os.ReadFile(filepath.Join(c.Config.Config.Cwd, "marker"))
			if err != nil || string(marker) != "x" {
				t.Fatal("process was rerun", string(marker), err)
			}
		})
	}
}

func TestIncompleteArgumentsNeverReachRealRunner(t *testing.T) {
	_, remote, root := remoteRunner(t)
	script := fixture.New(fixture.Step{Events: []model.Delta{
		{Kind: model.CallStart, CallID: "partial", Name: "exec_command"},
		{Kind: model.ArgumentsDelta, CallID: "partial", Text: `{"command":"touch forbidden"`},
	}})
	c := loopCore(t, script)
	var err error
	c.Tools, err = tool.NewRegistry(tool.ProcessExecutors(remote)...)
	if err != nil {
		t.Fatal(err)
	}
	a := createWork(t, c, "partial call")
	runCore(t, c)
	waitTask(t, c, a, "failed")
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatal("partial arguments created a process", files, err)
	}
	if _, err := os.Stat(filepath.Join(c.Config.Config.Cwd, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("partial command executed", err)
	}
}
