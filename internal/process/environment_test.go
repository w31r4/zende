package process

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zende.local/harness/internal/protocol"
)

func TestProcessEnvironmentReplacesCaller(t *testing.T) {
	original := []string{"PATH=/test", protocol.CallerEnvironment + "=wrong", "LANG=en_US.UTF-8", protocol.CallerEnvironment + "=another"}
	spec := Spec{ID: "call", AgentID: "agent", TaskID: "task"}
	env := processEnvironment(spec, original)
	if len(env) != 3 || len(original) != 4 || env[0] != original[0] || env[1] != original[2] {
		t.Fatal(env, original)
	}
	var caller protocol.Caller
	if err := json.Unmarshal([]byte(strings.TrimPrefix(env[2], protocol.CallerEnvironment+"=")), &caller); err != nil || caller != (protocol.Caller{AgentID: "agent", TaskID: "task", OperationID: "call"}) {
		t.Fatal(caller, err)
	}
	if env := processEnvironment(Spec{}, original); len(env) != 2 {
		t.Fatal("unbound process inherited attribution", env)
	}
}

func TestPipeAndPTYReceiveOwnCaller(t *testing.T) {
	t.Setenv(protocol.CallerEnvironment, "stale-runner-caller")
	for _, pty := range []bool{false, true} {
		name := "pipe"
		if pty {
			name = "pty"
		}
		t.Run(name, func(t *testing.T) {
			m := manager(t, 4096)
			if pty {
				m = tmuxManager(t, 4096)
			}
			ctx := context.Background()
			spec := Spec{ID: "host-call", AgentID: "agent-identity", TaskID: "task-identity", Cwd: t.TempDir(), PTY: pty, Command: "printf '%s' \"$ZENDE_CALLER\""}
			if _, err := m.Start(ctx, spec); err != nil {
				t.Fatal(err)
			}
			done, err := m.Wait(ctx, spec.ID, 5*time.Second)
			if err != nil || done.Status != "exited" || done.ExitCode == nil || *done.ExitCode != 0 {
				t.Fatal(done, err)
			}
			stream := "stdout"
			if pty {
				stream = "pty"
			}
			out, err := m.Read(ctx, spec.ID, stream, 0, 4096)
			if err != nil {
				t.Fatal(err)
			}
			var caller protocol.Caller
			if err := json.Unmarshal(out.Data, &caller); err != nil || caller != (protocol.Caller{AgentID: spec.AgentID, TaskID: spec.TaskID, OperationID: spec.ID}) {
				t.Fatal(string(out.Data), err)
			}
		})
	}
}
