package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/render"
)

type projectionSource struct{ events []protocol.Event }

func (s projectionSource) Handle(_ context.Context, req protocol.Request) (any, error) {
	if req.Method != "agent.history" {
		return nil, protocol.E("invalid_request", "fixture is read-only")
	}
	return protocol.EventPage{Events: s.events, Next: 16, Latest: 16}, nil
}
func (s projectionSource) Subscribe(context.Context, protocol.Request, func(protocol.Event) error) error {
	return nil
}

func TestCLIHistoryMatchesTUIProjection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_RUNTIME_DIR", dir)
	a := &app{profile: "projection"}
	paths, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../render/testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	server, err := protocol.Listen(paths.Socket, projectionSource{events})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	var out, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"--profile", "projection", "agent", "history", "agent"}, nil, &out, &diagnostics, BuildInfo{}); code != 0 {
		t.Fatal(code, diagnostics.String())
	}
	var page protocol.EventPage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, page.Events) {
		t.Fatal("CLI changed fixture event identities or content")
	}
	cliView := render.Transcript{Agent: protocol.Agent{ID: "agent"}}
	tuiView := render.Transcript{Agent: protocol.Agent{ID: "agent"}}
	for _, event := range page.Events {
		if err := cliView.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range events {
		if err := tuiView.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(cliView, tuiView) || cliView.Cursor != page.Next || len(cliView.Entries) != 4 || cliView.Entries[0].State != "completed" || cliView.Entries[2].OperationID != "operation" || cliView.Entries[2].CallID != "call" || cliView.Entries[2].ProcessID != "process" || cliView.Entries[2].State != "completed" {
		t.Fatal("CLI/TUI fact mismatch")
	}
	_ = tuiView.Lines(48, 13, 0, true)
	if !reflect.DeepEqual(cliView, tuiView) {
		t.Fatal("layout changed facts")
	}
}
