package render

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

func TestReconnectKeepsCursorDraftAndPendingIdentity(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent"}, Cursor: 16}})
	first := m.generation
	for _, event := range historyFixture(t)[:10] {
		m, _ = update(t, m, watchEventMsg{event, first})
	}
	m.input.SetValue("draft")
	req, _ := protocol.NewRequest("agent.submit", "unresolved", protocol.Submit{AgentID: "agent", Input: "original"})
	m.pending = &req
	m, cmd := update(t, m, watchEndedMsg{io.EOF, first})
	if cmd == nil || m.watching || m.connected || !m.retryPending || m.cursor != 10 {
		t.Fatal("missing reconnect", m.cursor, m.retryPending)
	}
	m, cmd = update(t, m, reconnectMsg{first})
	if cmd == nil || !m.watching || m.generation == first || m.cursor != 10 || m.input.Value() != "draft" || m.pending.OperationID != "unresolved" {
		t.Fatal("reconnect lost local state")
	}
	second := m.generation
	m, _ = update(t, m, watchEndedMsg{io.EOF, first})
	m, _ = update(t, m, watchEventMsg{historyFixture(t)[10], first})
	if !m.watching || m.cursor != 10 {
		t.Fatal("obsolete stream changed current state")
	}
	m, _ = update(t, m, watchReadyMsg{second})
	if !m.connected || m.connectionErr != nil {
		t.Fatal("idle subscription ack not connected")
	}
	for _, event := range historyFixture(t)[8:] {
		m, _ = update(t, m, watchEventMsg{event, second})
	}
	if m.cursor != 16 || len(m.transcript.Entries) != 4 || m.transcript.Entries[0].State != "completed" || m.pending.OperationID != "unresolved" {
		t.Fatal("missed/duplicated replay")
	}
}

func TestGapCannotBeHiddenBySnapshotOrStaleWatch(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m, _ = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent"}, Cursor: 16}})
	old := m.generation
	m, _ = update(t, m, watchEventMsg{historyFixture(t)[0], old})
	m, cmd := update(t, m, watchEventMsg{historyFixture(t)[2], old})
	if cmd != nil || !m.gap || m.watching || m.cursor != 1 {
		t.Fatal("gap silently skipped")
	}
	m, _ = update(t, m, watchEndedMsg{io.EOF, old})
	m, cmd = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent"}, Cursor: 100}})
	if cmd != nil || m.watching || m.cursor != 1 || !m.gap {
		t.Fatal("snapshot erased gap")
	}
	m.input.SetValue("/refresh")
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, cmd = update(t, m, snapshotMsg{Snapshot: protocol.Snapshot{Agent: protocol.Agent{ID: "agent"}, Cursor: 100}})
	if cmd == nil || m.gap || !m.watching || m.cursor != 1 {
		t.Fatal("manual retry did not keep cursor")
	}
}

func TestInitialFailureRetriesAndTimerCancels(t *testing.T) {
	m := New(context.Background(), panicClient{}, "agent", func(tea.Msg) {})
	m, cmd := update(t, m, snapshotMsg{Err: errors.New("socket offline")})
	if cmd == nil || !m.retryPending || m.haveSnapshot {
		t.Fatal("initial failure not retried")
	}
	m, cmd = update(t, m, reconnectMsg{m.generation})
	if cmd == nil || !m.refreshing || m.watching {
		t.Fatal("initial retry must inspect before subscribing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan tea.Msg, 1)
	go func() { done <- reconnectCmd(ctx, 0, time.Hour)() }()
	select {
	case msg := <-done:
		if msg != nil {
			t.Fatal("cancelled timer emitted reconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("timer leaked")
	}
}
