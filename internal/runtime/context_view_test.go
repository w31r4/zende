package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

func TestModelRequestsPinContextViewAcrossPublication(t *testing.T) {
	ctx := context.Background()
	var core *Core
	var agent string
	published := make(chan store.ContextView, 1)
	script := fixture.New(
		fixture.Step{Events: fixture.Answer("commentary", "first result"), Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[0].Content[0].Text, "first summary") {
				return fmt.Errorf("first request did not use current view")
			}
			before, _ := json.Marshal(request.Messages)
			snap, err := core.Store.ContextSnapshot(ctx, agent)
			if err != nil {
				return err
			}
			events, err := core.Store.Events(ctx, protocol.Cursor{AgentID: agent, Limit: 1000})
			if err != nil {
				return err
			}
			var through int64
			for _, event := range events.Events {
				if event.Kind == "context.message" {
					through = event.Seq
				}
			}
			view, err := core.Store.PublishContextView(ctx, agent, store.ContextView{Previous: snap.View.ID, SourceCursor: snap.Cursor, Through: through, Summary: "second summary"})
			if err != nil {
				return err
			}
			after, _ := json.Marshal(request.Messages)
			if string(before) != string(after) {
				return fmt.Errorf("publication changed an in-flight request")
			}
			published <- view
			return nil
		}},
		fixture.Step{Events: fixture.Answer("final", "second result"), Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[0].Content[0].Text, "second summary") {
				return fmt.Errorf("next request retained old view")
			}
			foundTail := false
			for _, message := range request.Messages {
				for _, b := range message.Content {
					if b.Text == "first result" {
						foundTail = true
					}
					if b.Text == "raw old answer" {
						return fmt.Errorf("request expanded summarized raw history")
					}
				}
			}
			if !foundTail {
				return fmt.Errorf("concurrent response tail was dropped")
			}
			if request.Messages[len(request.Messages)-1].Role != "developer" {
				return fmt.Errorf("current settings were lost in summary")
			}
			return nil
		}},
	)
	core = loopCore(t, script)
	first := createWork(t, core, "first task")
	agent = first.AgentID
	var end int64
	for _, value := range []struct{ role, text string }{{"user", "raw old question"}, {"assistant", "raw old answer"}} {
		event, err := core.Store.Append(ctx, agent, "", "context.message", store.ContextMessage{Message: model.Message{Role: value.role, Content: []model.Block{{Type: "text", Text: value.text}}}})
		if err != nil {
			t.Fatal(err)
		}
		end = event.Seq
	}
	initial, err := core.Store.PublishContextView(ctx, agent, store.ContextView{SourceCursor: end, Through: end, Summary: "first summary"})
	if err != nil {
		t.Fatal(err)
	}
	runCore(t, core)
	waitTask(t, core, first, "completed")
	var next store.ContextView
	select {
	case next = <-published:
	default:
		t.Fatal("no replacement view")
	}
	events, err := core.Store.Events(ctx, protocol.Cursor{AgentID: agent, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	views := []string{}
	for _, event := range events.Events {
		if event.Kind == "model.started" {
			var request requestRecord
			if err := json.Unmarshal(event.Payload, &request); err != nil {
				t.Fatal(err)
			}
			views = append(views, request.ViewID)
		}
	}
	if len(views) != 2 || views[0] != initial.ID || views[1] != next.ID {
		t.Fatal("request provenance changed", views)
	}
}
