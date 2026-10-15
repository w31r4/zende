package render

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/provider"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/runtime"
	"zende.local/harness/internal/store"
)

type selectionProbe struct{ reply chan selectionUIState }
type selectionUIState struct {
	kind, path, cwd, model, draft, git, errorKind string
	ready, pending, gitWaiting                    bool
}

func TestRealTerminalSelectsThroughCoreAndRefreshesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the working-tree integration test")
	}
	dir, err := os.MkdirTemp("/tmp", "zende-select-ui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	work, outside := filepath.Join(dir, "work"), filepath.Join(dir, "中文 outside")
	for _, path := range []string{filepath.Join(work, "child"), outside} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	git := exec.Command("git", "-c", "init.defaultBranch=day1", "init", "--quiet", work)
	git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := resource.Defaults(work)
	cfg.Connections["local"] = resource.Connection{Protocol: "responses", BaseURL: "https://test.invalid/v1", Credential: "env:UNRESOLVED_TEST_KEY",
		Models: []resource.ModelConfig{{ID: "configured", ContextWindow: 8192, MaxOutput: 1024, Efforts: []string{"high"}}}}
	var credentialReads atomic.Int32
	resolver, err := provider.New(cfg, func(string) string { credentialReads.Add(1); return "" })
	if err != nil {
		t.Fatal(err)
	}
	core := runtime.New(s, resource.Effective{Config: cfg})
	core.Catalog, core.RestoreModels = resolver, resolver
	server, err := protocol.Listen(filepath.Join(dir, "core.sock"), core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("core did not shut down")
		}
	}()
	client := protocol.Client{Socket: filepath.Join(dir, "core.sock")}
	req, _ := protocol.NewRequest("agent.create", "initial", protocol.CreateAgent{Cwd: work})
	var created protocol.Accepted
	if err := client.Call(ctx, req, &created); err != nil {
		t.Fatal(err)
	}
	readChoice := func() protocol.AgentSelection {
		t.Helper()
		req, _ := protocol.NewRequest("agent.selection", "", protocol.Target{AgentID: created.AgentID})
		var choice protocol.AgentSelection
		if err := client.Call(ctx, req, &choice); err != nil {
			t.Fatal(err)
		}
		return choice
	}
	initial := readChoice()
	master, slave, capture := testTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	uiCtx, stopUI := context.WithCancel(ctx)
	defer stopUI()
	var program *tea.Program
	m := New(uiCtx, client, created.AgentID, func(msg tea.Msg) { program.Send(msg) })
	program = tea.NewProgram(m, tea.WithContext(uiCtx), tea.WithInput(slave), tea.WithOutput(slave), tea.WithWindowSize(48, 16),
		tea.WithColorProfile(colorprofile.ASCII), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
			if probe, ok := msg.(selectionProbe); ok {
				m := model.(Shell)
				state := selectionUIState{cwd: m.snapshot.Agent.Cwd, model: m.snapshot.Agent.Model, draft: m.input.Value(), pending: m.pending != nil,
					git: m.git.State, gitWaiting: m.gitWaiting}
				if m.selection != nil {
					state.kind, state.ready, state.path = m.selection.kind, m.selection.ready, m.selection.directory.Path
				}
				if err, ok := m.err.(*protocol.Error); ok {
					state.errorKind = err.Data.Kind
				}
				probe.reply <- state
				return nil
			}
			return msg
		}))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	defer func() { program.Kill(); program.Wait() }()
	waitTerminal(t, capture, "\x1b[?1049h")
	waitState := func(check func(selectionUIState) bool) selectionUIState {
		t.Helper()
		var state selectionUIState
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			reply := make(chan selectionUIState, 1)
			program.Send(selectionProbe{reply})
			select {
			case state = <-reply:
				if check(state) {
					return state
				}
			case <-time.After(time.Second):
				t.Fatal("UI probe timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("missing UI state", state)
		return state
	}
	write := func(text string) {
		t.Helper()
		if _, err := master.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	waitState(func(s selectionUIState) bool { return s.cwd == work && s.git == "ready" })
	const draft = "中文 draft "
	write("\x1b[200~" + draft + "\x1b[201~/model\r")
	waitState(func(s selectionUIState) bool { return s.kind == "connections" && s.ready && s.draft == draft })
	write("\r")
	waitState(func(s selectionUIState) bool { return s.kind == "models" && s.ready })
	write("\r")
	waitState(func(s selectionUIState) bool { return s.kind == "efforts" })
	write("j\r") // Configured high effort, not a guessed provider capability.
	waitState(func(s selectionUIState) bool { return s.kind == "confirm" })
	if readChoice() != initial {
		t.Fatal("browsing committed before confirmation")
	}
	write("\x1b")
	waitState(func(s selectionUIState) bool { return s.kind == "" && s.draft == draft })
	if readChoice() != initial {
		t.Fatal("cancel changed selection")
	}
	write("/model\r")
	waitState(func(s selectionUIState) bool { return s.kind == "connections" && s.ready })
	write("\r")
	waitState(func(s selectionUIState) bool { return s.kind == "models" && s.ready })
	write("\r")
	waitState(func(s selectionUIState) bool { return s.kind == "efforts" })
	write("j\r")
	waitState(func(s selectionUIState) bool { return s.kind == "confirm" })
	write("\r")
	waitState(func(s selectionUIState) bool {
		return s.kind == "" && !s.pending && s.model == "local/configured" && s.draft == draft
	})
	choice := readChoice()
	if choice.Revision <= initial.Revision || choice.ModelRevision != choice.Revision || choice.Effort != "high" {
		t.Fatal(choice)
	}
	write("/cd\r")
	waitState(func(s selectionUIState) bool { return s.kind == "directory" && s.ready && s.path == work })
	// Dot, dot-dot, .git, child: explicit down/down/down then l.
	write("jjjl")
	waitState(func(s selectionUIState) bool { return s.ready && s.path == filepath.Join(work, "child") })
	write("h")
	waitState(func(s selectionUIState) bool { return s.ready && s.path == work })
	if readChoice() != choice {
		t.Fatal("navigation changed cwd")
	}
	write("/cd '" + outside + "'\r")
	waitState(func(s selectionUIState) bool { return s.ready && s.path == outside })
	write("\r") // The first row is '.'.
	waitState(func(s selectionUIState) bool {
		return s.kind == "" && !s.pending && s.cwd == outside && s.git == "none" && !s.gitWaiting && s.draft == draft
	})
	final := readChoice()
	if final.Cwd != outside || final.Model != choice.Model || final.Effort != choice.Effort || final.Revision <= choice.Revision || credentialReads.Load() != 0 {
		t.Fatal("persisted choice or credential isolation failed", final, credentialReads.Load())
	}
	write("/git\r")
	waitState(func(s selectionUIState) bool { return s.kind == "git" && s.git == "none" && !s.gitWaiting })
	waitTerminal(t, capture, "Not a Git repository")
	write("\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach timed out")
	}
	waitTerminal(t, capture, "\x1b[?1049l")
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || !reflect.DeepEqual(before, after) || readChoice() != final {
		t.Fatal("detach altered terminal or durable selection", err)
	}
	snapshot, err := s.Snapshot(ctx, created.AgentID)
	if err != nil || len(snapshot.Tasks) != 0 {
		t.Fatal("navigation submitted the held draft", snapshot, err)
	}
	history, err := s.Events(ctx, protocol.Cursor{AgentID: created.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	selections := 0
	for _, event := range history.Events {
		if event.Kind == "agent.selection" {
			selections++
		}
	}
	if selections != 2 {
		t.Fatal("expected exactly model and directory confirmations", selections)
	}
}
