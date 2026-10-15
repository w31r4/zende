package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// ContextView is an append-only projection, not a replacement for raw history.
// Through is an inclusive raw context.message cursor. Later messages remain live.
type ContextView struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	Previous     string `json:"previous,omitempty"`
	SourceCursor int64  `json:"source_cursor"`
	Through      int64  `json:"through"`
	Summary      string `json:"summary"`
	Published    int64  `json:"published,omitempty"`
}

func (v ContextView) message() model.Message {
	return model.Message{Role: "user", Content: []model.Block{{Type: "text", Text: fmt.Sprintf("Summary of earlier context (Agent %s; view %s; raw context through event %d):\n%s", v.AgentID, v.ID, v.Through, v.Summary)}}}
}

type ContextSnapshot struct {
	Messages []model.Message
	Cursor   int64
	View     ContextView
}

func (s *Store) ContextSnapshot(ctx context.Context, agent string) (ContextSnapshot, error) {
	snapshot := ContextSnapshot{Messages: []model.Message{}}
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM agents WHERE id=?", agent).Scan(&snapshot.Cursor); err != nil {
			return err
		}
		view, err := contextView(ctx, tx, agent, snapshot.Cursor)
		if err != nil {
			return err
		}
		snapshot.View = view
		entries, err := contextEntries(ctx, tx, agent, snapshot.Cursor)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			snapshot.Messages = append(snapshot.Messages, entry.Record.Message)
		}
		return nil
	})
	return snapshot, err
}

func contextView(ctx context.Context, tx *sql.Tx, agent string, cursor int64) (ContextView, error) {
	var view ContextView
	var data []byte
	var published int64
	err := tx.QueryRowContext(ctx, "SELECT seq,payload FROM events WHERE agent_id=? AND kind='context.view' AND seq<=? ORDER BY seq DESC LIMIT 1", agent, cursor).Scan(&published, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	if err := json.Unmarshal(data, &view); err != nil {
		return view, err
	}
	view.Published = published
	return view, nil
}

// PublishContextView only installs a prepared summary. Generation, capacity
// triggers and a public compact operation belong to the compaction coordinator.
// A concurrent tail is retained; a competing view invalidates this preparation.
func (s *Store) PublishContextView(ctx context.Context, agent string, prepared ContextView) (ContextView, error) {
	var published ContextView
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if strings.TrimSpace(prepared.Summary) == "" || !utf8.ValidString(prepared.Summary) || len(prepared.Summary) > 1<<20 || prepared.Through <= 0 || prepared.Through > prepared.SourceCursor {
			return protocol.E("context_invalid", "summary and a valid source range are required")
		}
		var cursor int64
		var state string
		if err := tx.QueryRowContext(ctx, "SELECT seq,state FROM agents WHERE id=?", agent).Scan(&cursor, &state); err != nil {
			return err
		}
		if state == "copying" || state == "archived" {
			return protocol.E("conflict", "agent is read-only")
		}
		if prepared.SourceCursor > cursor {
			return protocol.E("context_invalid", "source cursor is in the future")
		}
		current, err := contextView(ctx, tx, agent, cursor)
		if err != nil {
			return err
		}
		source, err := contextView(ctx, tx, agent, prepared.SourceCursor)
		if err != nil {
			return err
		}
		if current.ID != prepared.Previous || source.ID != prepared.Previous {
			return protocol.E("context_conflict", "context view changed; prepare a new summary")
		}
		if prepared.Through <= source.Through {
			return protocol.E("context_invalid", "summary must cover a new complete prefix")
		}
		entries, err := contextEntries(ctx, tx, agent, prepared.SourceCursor)
		if err != nil {
			return err
		}
		end := 0
		for end < len(entries) && entries[end].Seq <= prepared.Through {
			end++
		}
		if end == 0 || entries[end-1].Seq != prepared.Through || entries[end-1].ViewID != "" {
			return protocol.E("context_invalid", "summary endpoint must be a raw context message")
		}
		complete, err := completePrefix(entries[:end])
		if err != nil {
			return err
		}
		if complete != end {
			return protocol.E("context_invalid", "summary would split a tool call and result")
		}
		prepared.ID, prepared.AgentID, prepared.Published = protocol.NewID("view"), agent, 0
		event, err := appendEvent(ctx, tx, agent, "", "context.view", prepared)
		if err != nil {
			return err
		}
		prepared.Published = event.Seq
		published = prepared
		return nil
	})
	if err != nil {
		return ContextView{}, err
	}
	return published, nil
}
