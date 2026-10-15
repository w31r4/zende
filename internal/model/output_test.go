package model_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/model/fixture"
)

func classify(t *testing.T, events []model.Delta, native bool) (model.Response, []model.PublicDelta, error) {
	t.Helper()
	var collector model.Collector
	c := model.Classifier{Native: native}
	var deltas []model.PublicDelta
	for _, event := range events {
		if err := collector.Add(event); err != nil {
			t.Fatal(err)
		}
		changes, _ := c.Add(event)
		deltas = append(deltas, changes...)
	}
	r, err := collector.Response()
	if err != nil {
		t.Fatal(err)
	}
	r.Output, err = c.Output()
	if err == nil {
		_, _, err = r.Final()
	}
	return r, deltas, err
}

func markedEvents(text string, chunks []int) []model.Delta {
	events := []model.Delta{{Kind: model.MessageStart, MessageID: "answer"}}
	pos := 0
	for _, end := range chunks {
		events = append(events, model.Delta{Kind: model.TextDelta, MessageID: "answer", Text: text[pos:end]})
		pos = end
	}
	if pos < len(text) {
		events = append(events, model.Delta{Kind: model.TextDelta, MessageID: "answer", Text: text[pos:]})
	}
	return append(events, model.Delta{Kind: model.MessageEnd, MessageID: "answer"}, model.Delta{Kind: model.ResponseEnd})
}

func TestExplicitOutputAcrossEveryByteBoundaryAndLiterals(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		commentary := "检查中文与 🟩\n> <final>\n\\<final>\n <final>\n```xml\n<final>\n</final>\n```\n~~~~\n<final>\n~~~\n</final>\n~~~~\n"
		text := "<commentary>\n" + commentary + "</commentary>\n<commentary>\n第二段\n</commentary>\n<final>\n完成\n</final>"
		text = strings.ReplaceAll(text, "\n", newline)
		want := []model.PublicMessage{{MessageID: "answer", Index: 0, Category: "commentary", Text: strings.ReplaceAll(commentary, "\n", newline)}, {MessageID: "answer", Index: 1, Category: "commentary", Text: "第二段" + newline}, {MessageID: "answer", Index: 2, Category: "final", Text: "完成" + newline}}
		check := func(chunks []int) {
			r, _, err := classify(t, markedEvents(text, chunks), false)
			if err != nil || !reflect.DeepEqual(r.Output, want) {
				t.Fatalf("chunks=%v output=%+v err=%v", chunks, r.Output, err)
			}
			if r.Messages[0].Content[0].Text != text {
				t.Fatal("classification changed original text")
			}
		}
		for i := 0; i <= len(text); i++ {
			check([]int{i})
		}
		var bytes []int
		for i := 1; i <= len(text); i++ {
			bytes = append(bytes, i)
		}
		check(bytes)
	}
}

func TestInvalidMarkedOutputNeverBecomesFinal(t *testing.T) {
	for _, text := range []string{
		"finished", "<final>\nno close", "<final>\nnested\n<commentary>\n</commentary>\n</final>",
		"<final>\ndone\n</final>\n<final>\nagain\n</final>", "<final>\ndone\n</final>\nafter",
		"before\n<final>\ndone\n</final>", "<commentary>\nwrong\n</final>", "</final>",
		"<final>inline</final>", " <final>\nx\n </final>", "<final> \nx\n</final>",
		"<final>\n```\n</final>",
	} {
		_, _, err := classify(t, markedEvents(text, nil), false)
		if err == nil {
			t.Fatalf("accepted %.100q", text)
		}
	}
	// Multiple source messages must obey the same single-final rule.
	events := fixture.Answer("final", "one")
	events = events[:len(events)-1]
	other := fixture.Answer("final", "two")
	for i := range other {
		if other[i].MessageID != "" {
			other[i].MessageID = "other"
		}
	}
	events = append(events, other...)
	if _, _, err := classify(t, events, true); err == nil {
		t.Fatal("two native finals accepted")
	}
}

func TestNativeCategoriesAreExplicitAndPrivateDataIsUnchanged(t *testing.T) {
	text := "中文 🟩 <final> is literal"
	events := []model.Delta{{Kind: model.MessageStart, MessageID: "answer", Category: "final"}}
	for i := range len(text) {
		events = append(events, model.Delta{Kind: model.TextDelta, MessageID: "answer", Text: text[i : i+1]})
	}
	events = append(events, model.Delta{Kind: model.ProtocolData, MessageID: "answer", Protocol: "fixture", Raw: json.RawMessage(`{"signature":"private","continuation":[1,2]}`)}, model.Delta{Kind: model.MessageEnd, MessageID: "answer"}, model.Delta{Kind: model.ResponseEnd})
	r, deltas, err := classify(t, events, true)
	if err != nil || len(r.Output) != 1 || r.Output[0].Text != text || r.Messages[0].Protocol != "fixture" || string(r.Messages[0].Opaque) != `{"signature":"private","continuation":[1,2]}` {
		t.Fatal(r, err)
	}
	var streamed string
	for _, delta := range deltas {
		if delta.Kind == "text" {
			streamed += delta.Text
		}
	}
	if streamed != text {
		t.Fatal(streamed)
	}
	if _, _, err := classify(t, fixture.Answer("", "seems final"), true); err == nil {
		t.Fatal("inferred native category")
	}
	if _, _, err := classify(t, fixture.Answer("final", "no tags"), false); err == nil {
		t.Fatal("trusted an undeclared native category")
	}
	// The fallback also leaves an opaque field on its original tagged message.
	events = markedEvents("<final>\ntext\n</final>", nil)
	beforeEnd := len(events) - 2
	events = append(events[:beforeEnd], append([]model.Delta{{Kind: model.ProtocolData, MessageID: "answer", Protocol: "fixture", Raw: json.RawMessage(`{"signature":"exact"}`)}}, events[beforeEnd:]...)...)
	r, _, err = classify(t, events, false)
	if err != nil || string(r.Messages[0].Opaque) != `{"signature":"exact"}` || r.Messages[0].Content[0].Text != "<final>\ntext\n</final>" {
		t.Fatal(r, err)
	}
}

func TestFinalToolsConflictAndStreamingIsProvisional(t *testing.T) {
	events := markedEvents("<final>\ndone\n</final>\n", nil)
	events = append(events[:len(events)-1], fixture.Call("call", "act", `{}`)...)
	r, deltas, err := classify(t, events, false)
	if err == nil || len(r.Calls) != 1 {
		t.Fatal(r, err)
	}
	if len(deltas) != 3 || deltas[2].Kind != "end" {
		t.Fatal(deltas)
	}
	// A closed final plus a response interrupted before ResponseEnd isn't usable.
	var collector model.Collector
	for _, event := range markedEvents("<final>\ndone\n</final>", nil)[:3] {
		if err := collector.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collector.Response(); err == nil {
		t.Fatal("unfinished transport completed")
	}
}

func TestOutputBatchingAndLimits(t *testing.T) {
	text := "<final>\n" + strings.Repeat("x\n", 100000) + "</final>"
	_, deltas, err := classify(t, markedEvents(text, nil), false)
	if err != nil || len(deltas) != 3 {
		t.Fatal(len(deltas), err)
	}
	_, _, err = classify(t, markedEvents(strings.Repeat("<commentary>\nx\n</commentary>\n", model.MaxResponseBlocks+1), nil), false)
	if err == nil {
		t.Fatal("classification expansion not bounded")
	}
}

func TestLateNativeCategoryWaitsWithoutGuessing(t *testing.T) {
	var c model.Classifier
	c.Native = true
	for _, d := range []model.Delta{{Kind: model.MessageStart, MessageID: "m"}, {Kind: model.TextDelta, MessageID: "m", Text: "中文"}} {
		out, err := c.Add(d)
		if err != nil || len(out) != 0 {
			t.Fatal("uncategorized text published", out, err)
		}
	}
	out, err := c.Add(model.Delta{Kind: model.MessageCategory, MessageID: "m", Category: "final"})
	if err != nil || len(out) != 2 || out[1].Text != "中文" {
		t.Fatal(out, err)
	}
	_, err = c.Add(model.Delta{Kind: model.MessageEnd, MessageID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := c.Output()
	if err != nil || len(output) != 1 || output[0].Category != "final" {
		t.Fatal(output, err)
	}
}

func FuzzOutputChunkingPreservesClassification(f *testing.F) {
	for _, seed := range []string{"<final>\n完成 🟩\n</final>", "<commentary>\n```xml\n<final>\n```\n</commentary>", "<final>\r\nno close", "<final>\n\xff\n</final>"} {
		f.Add(seed, uint8(1))
	}
	f.Fuzz(func(t *testing.T, text string, width uint8) {
		if len(text) > 65536 {
			t.Skip()
		}
		run := func(step int) ([]model.PublicMessage, error) {
			var c model.Classifier
			_, _ = c.Add(model.Delta{Kind: model.MessageStart, MessageID: "answer"})
			for start := 0; start < len(text); start += step {
				end := min(start+step, len(text))
				_, _ = c.Add(model.Delta{Kind: model.TextDelta, MessageID: "answer", Text: text[start:end]})
			}
			_, _ = c.Add(model.Delta{Kind: model.MessageEnd, MessageID: "answer"})
			return c.Output()
		}
		whole, wholeErr := run(max(1, len(text)))
		split, splitErr := run(int(width) + 1)
		if (wholeErr == nil) != (splitErr == nil) || wholeErr == nil && !reflect.DeepEqual(whole, split) {
			t.Fatalf("chunk-dependent classification: %v %v", wholeErr, splitErr)
		}
	})
}
