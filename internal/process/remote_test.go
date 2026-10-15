package process

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/protocol"
)

func TestRemoteInputRetainsPartialWriteAndReplayResult(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	if _, err := m.Start(ctx, Spec{ID: "remote-partial", Cwd: t.TempDir(), Command: "sleep 30"}); err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "remote-partial")
	e, _ := m.get("remote-partial")
	raw, err := e.stdin.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var resizeErr error
	if err := raw.Control(func(fd uintptr) { _, resizeErr = unix.FcntlInt(fd, unix.F_SETPIPE_SZ, 4096) }); err != nil {
		t.Fatal(err)
	}
	if resizeErr != nil {
		t.Fatal(resizeErr)
	}
	socket := filepath.Join(t.TempDir(), "runner.sock")
	server, err := protocol.Listen(socket, Service{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	defer func() { stop(); <-done }()
	remote := Remote{Client: protocol.Client{Socket: socket}}
	input := InputParams{ID: "remote-partial", Data: make([]byte, MaxInputBytes)}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := remote.Input(requestCtx, "partial-once", input)
	if err == nil || !strings.Contains(err.Error(), "input_partial") || result.OperationID != "partial-once" || result.Written <= 0 || result.Written >= MaxInputBytes {
		t.Fatal(result, err)
	}
	again, err := remote.Input(requestCtx, "partial-once", input)
	if err == nil || again.Written != result.Written {
		t.Fatal(again, err)
	}
	state, err := m.Inspect(ctx, input.ID)
	if err != nil || state.InputBytes != int64(result.Written) || state.Status != "running" {
		t.Fatal("partial input replayed or process killed", state, err)
	}
	record, err := remote.ControlInspect(ctx, ControlTarget{ID: input.ID, OperationID: "partial-once"})
	if err != nil || record.Status != "completed" || record.Result.Written != result.Written || record.Error == nil || record.Error.Data.Kind != "input_partial" {
		t.Fatal(record, err)
	}
}
