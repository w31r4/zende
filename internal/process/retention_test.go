package process

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"zende.local/harness/internal/protocol"
)

func completedOutput(t *testing.T, m *Manager, id, task, command string) State {
	t.Helper()
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: id, TaskID: task, Cwd: t.TempDir(), Command: command})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, id, 5*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	return state
}

func TestRotationPreservesTaskAndHistoryReferences(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	completedOutput(t, m, "task-output", "unfinished-task", "printf task")
	completedOutput(t, m, "history-output", "", "printf history")
	completedOutput(t, m, "eligible", "", "printf disposable")
	_, err := m.Reference(ctx, "history-pin", ReferenceParams{ID: "history-output", ReferenceID: "context-view", Kind: "history", Owner: "view-1", Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	// A process completing does not imply its owning Task completed.
	status, err := m.Retention(ctx)
	if err != nil || status.ProtectedBytes != 11 || status.EligibleBytes != 10 || status.HeldReferences != 2 {
		t.Fatal(status, err)
	}
	if _, err := m.Rotate(ctx, "too-small", RotateParams{RetainBytes: 10}); err == nil || !strings.Contains(err.Error(), "limit_exceeded") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.root, "eligible", "stdout")); err != nil {
		t.Fatal("failed capacity check deleted unrelated output", err)
	}
	record, err := m.Rotate(ctx, "safe-rotation", RotateParams{RetainBytes: 11})
	if err != nil || record.FreedBytes != 10 || record.RemainingBytes != 11 || len(record.Targets) != 1 || record.Targets[0].ID != "eligible" {
		t.Fatal(record, err)
	}
	for _, id := range []string{"task-output", "history-output"} {
		out, err := m.Read(ctx, id, "stdout", 0, 4096)
		if err != nil || len(out.Data) == 0 || out.Gap != nil {
			t.Fatal("referenced output lost", out, err)
		}
	}
	out, err := m.Read(ctx, "eligible", "stdout", 0, 4096)
	if err != nil || len(out.Data) != 0 || out.Gap == nil || out.Gap.From != 0 || out.Gap.To != 10 {
		t.Fatal("release not reported", out, err)
	}
	if _, err := m.Reference(ctx, "late-pin", ReferenceParams{ID: "eligible", ReferenceID: "too-late", Kind: "manual", Owner: "user", Hold: true}); err == nil {
		t.Fatal("deleted bytes falsely pinned")
	}
	state, err := m.Inspect(ctx, "eligible")
	if err != nil || state.ExitCode == nil || state.Command != "printf disposable" || state.ReleasedBy != "safe-rotation" {
		t.Fatal("rotation deleted process history", state, err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	status, err = reopened.Retention(ctx)
	if err != nil || status.ProtectedBytes != 11 || status.HeldReferences != 2 {
		t.Fatal(status, err)
	}
	again, err := reopened.Rotate(ctx, "safe-rotation", RotateParams{RetainBytes: 11})
	if err != nil || again.FreedBytes != record.FreedBytes || len(again.Targets) != 1 {
		t.Fatal(again, err)
	}
	if _, err := reopened.Rotate(ctx, "safe-rotation", RotateParams{RetainBytes: 0}); err == nil {
		t.Fatal("rotation request conflict accepted")
	}
}

func TestActiveOutputAndConcurrentReferenceAreProtected(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "active", Cwd: t.TempDir(), Command: "printf ready; read line"})
	if err != nil {
		t.Fatal(err)
	}
	stopTestProcess(t, m, "active")
	waitOutput(t, m, "active", "stdout", "ready")
	if _, err := m.Rotate(ctx, "active-prune", RotateParams{}); err == nil {
		t.Fatal("active output was removed")
	}
	completedOutput(t, m, "race", "", "printf retained")
	refDone := make(chan error, 1)
	go func() {
		_, err := m.Reference(ctx, "racing-pin", ReferenceParams{ID: "race", ReferenceID: "race-ref", Kind: "manual", Owner: "test", Hold: true})
		refDone <- err
	}()
	_, rotateErr := m.Rotate(ctx, "racing-rotation", RotateParams{RetainBytes: 5})
	refErr := <-refDone
	if refErr == nil {
		out, err := m.Read(ctx, "race", "stdout", 0, 4096)
		if err != nil || string(out.Data) != "retained" || out.Gap != nil {
			t.Fatal("successful reference lost output", out, err, rotateErr)
		}
	} else if !strings.Contains(refErr.Error(), "output_released") {
		t.Fatal(refErr)
	}
}

func TestRotationResumesOriginalPlanAfterTombstone(t *testing.T) {
	m := manager(t, 4096)
	ctx := context.Background()
	completedOutput(t, m, "planned", "", "printf old")
	e, _ := m.get("planned")
	// Fault window: release intent saved, data files not unlinked yet.
	e.mu.Lock()
	e.state.ReleasedBy = "interrupted"
	err := e.save()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.root, ".retention")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(dir, "interrupted.json"), Rotation{Version: 1, OperationID: "interrupted", Status: "pending", Targets: []RotationTarget{{ID: "planned", Bytes: 3}}}); err != nil {
		t.Fatal(err)
	}
	completedOutput(t, m, "later", "", "printf later")
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.Rotate(ctx, "interrupted", RotateParams{})
	if err != nil || record.FreedBytes != 3 || record.RemainingBytes != 5 || record.Status != "completed" {
		t.Fatal(record, err)
	}
	out, err := reopened.Read(ctx, "later", "stdout", 0, 4096)
	if err != nil || string(out.Data) != "later" {
		t.Fatal("retry selected new output", out, err)
	}
}

func TestOutputCacheBoundedAndCorruptionNotHidden(t *testing.T) {
	c := newOutputCache(1024)
	for i := range 300 {
		c.put(cacheKey{Offset: int64(i)}, make([]byte, 128))
	}
	if c.used > 1024 || c.lru.Len() > 8 {
		t.Fatal(c.used, c.lru.Len())
	}
	if _, ok := c.get(cacheKey{Offset: 0}); ok {
		t.Fatal("oldest block not evicted")
	}
	key := cacheKey{Path: "/test", Offset: 99}
	c.put(key, []byte("original"))
	b, _ := c.get(key)
	b[0] = 'x'
	b, _ = c.get(key)
	if string(b) != "original" {
		t.Fatal("cache alias")
	}
	m := manager(t, 4096)
	completedOutput(t, m, "cache", "", "printf data")
	ctx := context.Background()
	if _, err := m.Read(ctx, "cache", "stdout", 0, 4096); err != nil {
		t.Fatal(err)
	}
	if m.cache.used == 0 {
		t.Fatal("read did not fill cache")
	}
	path := filepath.Join(m.root, "cache", "stdout")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".replacement", []byte("edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path+".replacement", st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	if out, err := m.Read(ctx, "cache", "stdout", 0, 4096); err != nil || string(out.Data) != "edit" {
		t.Fatal("cache hid replaced file", out, err)
	}
	if err := os.Truncate(filepath.Join(m.root, "cache", "stdout"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(ctx, "cache", "stdout", 0, 4096); err == nil {
		t.Fatal("cache hid disk damage")
	}
}

func TestPTYOutputReferencesAndRotation(t *testing.T) {
	m := tmuxManager(t, 4096)
	ctx := context.Background()
	_, err := m.Start(ctx, Spec{ID: "pty-reference", TaskID: "task-pty", Cwd: t.TempDir(), PTY: true, Command: "printf kept"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Wait(ctx, "pty-reference", 5*time.Second)
	if err != nil || state.Status != "exited" {
		t.Fatal(state, err)
	}
	if _, err := m.PlanRotation(ctx, RotateParams{}); err == nil {
		t.Fatal("task reference ignored")
	}
	_, err = m.Reference(ctx, "task-done", ReferenceParams{ID: state.ID, ReferenceID: "task:task-pty", Kind: "task", Owner: "task-pty"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.Rotate(ctx, "pty-rotation", RotateParams{})
	if err != nil || record.FreedBytes != 4 {
		t.Fatal(record, err)
	}
	reopened, err := NewManager(m.root, m.shell, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reopened.Read(ctx, state.ID, "pty", 0, 4096)
	if err != nil || out.Gap == nil || out.Gap.To != 4 || len(out.Data) != 0 {
		t.Fatal("released PTY output reappeared", out, err)
	}
	state, err = reopened.Inspect(ctx, state.ID)
	if err != nil || state.Status != "exited" || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatal("rotation lost original exit", state, err)
	}
}

func TestDisplayTextEscapesTerminalControlAndBoundsFrame(t *testing.T) {
	raw := []byte("中文\n\t\x1b[2J\x1b]52;c;clipboard\a\r\x00\u009b31m\u202e")
	raw = append(raw, 0xff)
	text := DisplayText(raw)
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || r == '\u202e' {
			t.Fatalf("control escaped projection: %q", text)
		}
	}
	if !strings.Contains(text, "中文\n\t") || !strings.Contains(text, "\\x1b") || !strings.Contains(text, "\\xff") {
		t.Fatal(text)
	}
	large := make([]byte, 1<<20)
	encoded, err := json.Marshal(Output{Data: large, Text: DisplayText(large)})
	if err != nil || len(encoded) >= protocol.MaxFrameBytes {
		t.Fatal("escaped output exceeds IPC frame", len(encoded), err)
	}
	m := manager(t, 4096)
	completedOutput(t, m, "ansi", "", "printf '\033[2Jhello'")
	out, err := m.Read(context.Background(), "ansi", "stdout", 0, 4096)
	if err != nil || string(out.Data) != "\x1b[2Jhello" || out.Text != "\\x1b[2Jhello" {
		t.Fatal(out, err)
	}
}
