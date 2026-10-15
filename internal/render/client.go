package render

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"zende.local/harness/internal/protocol"
)

// Client is the existing core protocol, not another runtime owned by the UI.
type Client interface {
	Retry(context.Context, protocol.Request, any, int, time.Duration) error
	Watch(context.Context, protocol.Cursor, func(protocol.Event) error) error
}

type snapshotMsg struct {
	Snapshot protocol.Snapshot
	Err      error
}
type coreEventMsg struct{ Event protocol.Event }
type watchEventMsg struct {
	Event      protocol.Event
	Generation uint64
}
type watchReadyMsg struct{ Generation uint64 }
type watchEndedMsg struct {
	Err        error
	Generation uint64
}
type reconnectMsg struct{ Generation uint64 }
type acceptedMsg struct {
	Request  protocol.Request
	Accepted protocol.Accepted
	Err      error
}

func snapshotCmd(ctx context.Context, client Client, agent string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := protocol.NewRequest("agent.inspect", "", protocol.Target{AgentID: agent})
		var snapshot protocol.Snapshot
		if err == nil {
			err = client.Retry(ctx, req, &snapshot, 3, 100*time.Millisecond)
		}
		return snapshotMsg{snapshot, err}
	}
}

func mutationCmd(ctx context.Context, client Client, req protocol.Request) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var accepted protocol.Accepted
		err := client.Retry(ctx, req, &accepted, 3, 100*time.Millisecond)
		return acceptedMsg{req, accepted, err}
	}
}

// Bubble Tea owns this Cmd's goroutine. Sends provide backpressure; no detached
// goroutine or unbounded event queue is started from Update. Run cancels only
// this subscription on exit, never core work or runner processes.
func watchCmd(ctx context.Context, client Client, cursor protocol.Cursor, generation uint64, emit func(tea.Msg)) tea.Cmd {
	return func() tea.Msg {
		consume := func(event protocol.Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			emit(watchEventMsg{event, generation})
			return ctx.Err()
		}
		var err error
		if readyClient, ok := client.(interface {
			WatchReady(context.Context, protocol.Cursor, func(), func(protocol.Event) error) error
		}); ok {
			err = readyClient.WatchReady(ctx, cursor, func() { emit(watchReadyMsg{generation}) }, consume)
		} else {
			err = client.Watch(ctx, cursor, consume)
		}
		return watchEndedMsg{err, generation}
	}
}

func reconnectCmd(ctx context.Context, generation uint64, delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return reconnectMsg{generation}
		}
	}
}
