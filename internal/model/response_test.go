package model_test

import (
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
)

func collect(t *testing.T, events []model.Delta) model.Response {
	t.Helper()
	var collector model.Collector
	for _, event := range events {
		if err := collector.Add(event); err != nil {
			t.Fatal(event, err)
		}
	}
	r, err := collector.Response()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCollectorInterleavedCallsAndPrivateData(t *testing.T) {
	r := collect(t, []model.Delta{
		{Kind: model.MessageStart, MessageID: "note", Category: "commentary"},
		{Kind: model.TextDelta, MessageID: "note", Text: "正在检查"},
		{Kind: model.ProtocolData, MessageID: "note", Protocol: "fixture", Raw: []byte(`{"signature":"opaque"}`)},
		{Kind: model.MessageEnd, MessageID: "note"},
		{Kind: model.CallStart, CallID: "a", Name: "first"},
		{Kind: model.CallStart, CallID: "b", Name: "second"},
		{Kind: model.ArgumentsDelta, CallID: "b", Text: `{"x":`},
		{Kind: model.ArgumentsDelta, CallID: "a", Text: `{}`},
		{Kind: model.CallEnd, CallID: "a"},
		{Kind: model.ArgumentsDelta, CallID: "b", Text: `1}`},
		{Kind: model.CallEnd, CallID: "b"},
		{Kind: model.ResponseEnd},
	})
	if len(r.Messages) != 3 || len(r.Calls) != 2 || r.Calls[0].ID != "a" || string(r.Calls[1].Arguments) != `{"x":1}` || r.Usage != nil {
		t.Fatal(r)
	}
	if r.ValidateProtocol("fixture") != nil || r.ValidateProtocol("another") == nil {
		t.Fatal("private data protocol boundary")
	}
	if _, final, err := r.Final(); final || err != nil {
		t.Fatal(final, err)
	}
}

func TestCollectorRejectsIncompleteAndInvalidEvents(t *testing.T) {
	for name, events := range map[string][]model.Delta{
		"unfinished":        {{Kind: model.CallStart, CallID: "a", Name: "tool"}, {Kind: model.ArgumentsDelta, CallID: "a", Text: `{"x":`}},
		"early-end":         {{Kind: model.CallStart, CallID: "a", Name: "tool"}, {Kind: model.ResponseEnd}},
		"duplicate":         {{Kind: model.CallStart, CallID: "a", Name: "tool"}, {Kind: model.CallStart, CallID: "a", Name: "tool"}},
		"orphan":            {{Kind: model.ArgumentsDelta, CallID: "absent", Text: `{}`}},
		"changed-category":  {{Kind: model.MessageStart, MessageID: "m", Category: "commentary"}, {Kind: model.MessageCategory, MessageID: "m", Category: "final"}},
		"late-category":     {{Kind: model.MessageStart, MessageID: "m"}, {Kind: model.MessageEnd, MessageID: "m"}, {Kind: model.MessageCategory, MessageID: "m", Category: "final"}},
		"ambiguous-private": {{Kind: model.MessageStart, MessageID: "m"}, {Kind: model.CallStart, CallID: "a", Name: "tool"}, {Kind: model.ProtocolData, MessageID: "m", CallID: "a", Protocol: "fixture", Raw: []byte(`{}`)}},
		"duplicate-json":    fixture.Call("a", "tool", `{"x":1,"x":2}`),
		"null-json":         fixture.Call("a", "tool", `null`),
		"array-json":        fixture.Call("a", "tool", `[]`),
		"oversize":          fixture.Call("a", "tool", strings.Repeat("x", model.MaxBlockBytes+1)),
		"after-end":         append(fixture.Answer("final", "done"), model.Delta{Kind: model.TextDelta, MessageID: "answer", Text: "later"}),
	} {
		t.Run(name, func(t *testing.T) {
			var c model.Collector
			var err error
			for _, d := range events {
				if err = c.Add(d); err != nil {
					break
				}
			}
			if err == nil {
				_, err = c.Response()
			}
			if err == nil {
				t.Fatal("invalid sequence accepted")
			}
		})
	}
}

func TestFinalIsExplicitAndConflictsAreRejected(t *testing.T) {
	r := collect(t, fixture.Answer("", "looks finished"))
	if _, final, err := r.Final(); final || err != nil {
		t.Fatal(final, err)
	}
	r = collect(t, fixture.Answer("final", "done"))
	if text, final, err := r.Final(); !final || text != "done" || err != nil {
		t.Fatal(text, final, err)
	}
	for _, messages := range [][]model.Message{
		{{Category: "final"}, {Category: "commentary"}},
		{{Category: "final"}, {Category: "final"}},
	} {
		if _, _, err := (model.Response{Messages: messages}).Final(); err == nil {
			t.Fatal("conflict accepted")
		}
	}
	if _, _, err := (model.Response{Messages: []model.Message{{Category: "final"}}, Calls: []model.ToolCall{{ID: "a"}}}).Final(); err == nil {
		t.Fatal("final with tools accepted")
	}
}
