package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func seedSpawnTurns(t *testing.T, s *Store) (protocol.Accepted, []contextEntry) {
	t.Helper()
	_, parent := create(t, s)
	_, _ = s.ClaimNext(context.Background(), parent.AgentID)
	addSpawnContext(t, s, parent.AgentID, parent.TaskID,
		contextText("assistant", strings.Repeat("old response ", 100)),
		contextText("user", "second"),
		model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "one"}, {Type: "tool_call", CallID: "two"}}},
		contextText("user", "steer within open calls"),
		model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "one", Text: "one"}}},
		model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "two", Text: "two"}}},
		contextText("assistant", "second done"),
		contextText("user", "newest"),
		contextText("assistant", "newest done"),
		model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "pending"}}},
		contextText("assistant", "pending comment"))
	return parent, rawEntries(t, s, parent.AgentID)
}

func TestSpawnLastTurnsBudgetAndPendingPairs(t *testing.T) {
	s, _ := newStore(t)
	parent, raw := seedSpawnTurns(t, s)
	for _, tc := range []struct {
		mode                           string
		budget, first, messages, turns int
	}{
		{"all", 0, 0, 10, 4},
		{"last:1", 0, 8, 2, 1},
		{"last:2", 0, 2, 8, 3}, // the selected steer cannot orphan its open calls
		{"last:99", 0, 0, 10, 4},
		{"", estimateContext(raw[8:10]), 8, 2, 1},
		{"last:1", estimateContext(raw[8:10]), 8, 2, 1},
	} {
		t.Run(tc.mode+strconv.Itoa(tc.budget), func(t *testing.T) {
			child := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "work", Context: tc.mode, ContextBudget: tc.budget})
			source := child.Spawn
			if source.Messages != tc.messages || source.First != raw[tc.first].Seq || source.Last != raw[9].Seq || source.Turns != tc.turns || source.ExcludedPending != 2 || source.ExcludedSelection != tc.first || source.EstimatedTokens != estimateContext(raw[tc.first:10]) || source.EstimateMethod == "" {
				t.Fatal(source)
			}
			got := rawEntries(t, s, child.AgentID)
			for i := 0; i < tc.messages; i++ {
				if !reflect.DeepEqual(got[i].Record.Message, raw[i+tc.first].Record.Message) {
					t.Fatal("message split or changed", i)
				}
			}
			if n, err := completePrefix(got); err != nil || n != len(got) {
				t.Fatal(n, err)
			}
		})
	}
	// Explicit modes are strict, and every failure rolls back child and receipt.
	for _, p := range []protocol.SpawnAgent{
		{Context: "all", ContextBudget: estimateContext(raw[8:10])},
		{Context: "last:2", ContextBudget: estimateContext(raw[8:10])},
		{ContextBudget: 1},
		{Context: "none", ContextBudget: 1, Background: "too much"},
		{Context: "last:0"}, {Context: "last:-1"}, {Context: "last:01"},
		{Context: "budget"}, {ContextBudget: -1},
		{Background: string([]byte{255})},
	} {
		p.ParentID, p.Input = parent.AgentID, "rejected"
		req, _ := protocol.NewRequest("agent.spawn", protocol.NewID("fail"), p)
		before, _ := s.ListAgents(context.Background())
		if a, err := s.Spawn(context.Background(), req, p, "/", 0, 100, 10); err == nil || a.AgentID != "" {
			t.Fatal(p, a, err)
		}
		after, _ := s.ListAgents(context.Background())
		if !reflect.DeepEqual(before, after) {
			t.Fatal("partial spawn", p)
		}
		if _, found, err := s.AcceptedRequest(context.Background(), req); err != nil || found {
			t.Fatal("failed request has receipt", found, err)
		}
	}
}

func TestSpawnRawBackgroundAfterCompactionAndImages(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	parent, raw := seedSpawnTurns(t, s)
	snap, _ := s.ContextSnapshot(ctx, parent.AgentID)
	view, err := s.PublishContextView(ctx, parent.AgentID, ContextView{SourceCursor: snap.Cursor, Through: raw[7].Seq, Summary: "earlier rounds"})
	if err != nil {
		t.Fatal(err)
	}
	// An explicit original range can recover source messages omitted by a view.
	compressed := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "compressed"})
	if compressed.Spawn.Messages != 3 || !compressed.Spawn.SummaryIncluded || compressed.Spawn.ExcludedPending != 2 || compressed.Spawn.First != raw[8].Seq || compressed.Spawn.Last != raw[9].Seq {
		t.Fatal(compressed.Spawn)
	}
	recent := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "recent", Context: "last:1"})
	if recent.Spawn.Messages != 2 || recent.Spawn.SummaryIncluded || recent.Spawn.ViewID != view.ID || recent.Spawn.Turns != 1 {
		t.Fatal(recent.Spawn)
	}
	ref := protocol.ContextRange{AgentID: parent.AgentID, First: raw[2].Seq, Last: raw[7].Seq}
	child := spawnWithContext(t, s, protocol.SpawnAgent{ParentID: parent.AgentID, Input: "new task", Context: "none", Background: "Read these as evidence.", BackgroundHistory: []protocol.ContextRange{ref}})
	if child.Spawn.Messages != 0 || child.Spawn.ViewID != view.ID || child.Spawn.SummaryIncluded || child.Spawn.BackgroundMessages != 7 || !reflect.DeepEqual(child.Spawn.BackgroundHistory, []protocol.ContextRange{ref}) {
		t.Fatal(child.Spawn)
	}
	got := rawEntries(t, s, child.AgentID)
	for i := range 6 {
		if !reflect.DeepEqual(got[i].Record.Message, raw[i+2].Record.Message) {
			t.Fatal("raw reference was summarized", i)
		}
	}
	// Cross-Agent evidence owns its images independently of the parent.
	donorInput := protocol.CreateAgent{Cwd: t.TempDir(), Input: "donor"}
	donorRequest, _ := protocol.NewRequest("agent.create", "donor", donorInput)
	donor, err := s.Create(ctx, donorRequest, donorInput, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, image := saveImage(t, s, donor.AgentID, "evidence")
	private := model.Message{Role: "assistant", Protocol: "responses", Opaque: json.RawMessage(`{"type":"reasoning","encrypted_content":"private"}`)}
	addSpawnContext(t, s, donor.AgentID, "", model.Message{Role: "user", Content: []model.Block{{Type: "image", Attachment: image.Attachment.ID}}}, private)
	donorRaw := rawEntries(t, s, donor.AgentID)
	p := protocol.SpawnAgent{ParentID: parent.AgentID, Input: "image task", Context: "none", BackgroundHistory: []protocol.ContextRange{{AgentID: donor.AgentID, First: donorRaw[0].Seq, Last: donorRaw[1].Seq}}}
	imageChild := spawnWithContext(t, s, p)
	imageMessages := rawEntries(t, s, imageChild.AgentID)
	if !reflect.DeepEqual(imageMessages[1].Record.Message, private) || imageChild.Spawn.EstimatedTokens < 1200 {
		t.Fatal("private/image estimate lost")
	}
	if _, err := s.ReadImage(ctx, protocol.AttachmentTarget{AgentID: imageChild.AgentID, ID: imageMessages[0].Record.Message.Content[0].Attachment}); err != nil {
		t.Fatal(err)
	}
	// Invalid endpoints, orphan results, unfinished groups and duplicate calls.
	for _, refs := range [][]protocol.ContextRange{
		{{AgentID: parent.AgentID, First: 1, Last: raw[7].Seq}},
		{{AgentID: parent.AgentID, First: raw[5].Seq, Last: raw[7].Seq}},
		{{AgentID: parent.AgentID, First: raw[10].Seq, Last: raw[11].Seq}},
		{ref, ref},
	} {
		p.BackgroundHistory = refs
		req, _ := protocol.NewRequest("agent.spawn", protocol.NewID("invalid"), p)
		if _, err := s.Spawn(ctx, req, p, "/", 0, 100, 10); err == nil {
			t.Fatal("invalid background accepted", refs)
		}
	}
}
