// Package fixture supplies explicit, deterministic model fixtures for contract
// tests and local demonstrations. It is not a network provider or model.
package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"zende.local/harness/internal/model"
)

type Step struct {
	Events []model.Delta
	Check  func(model.Request) error
	Error  error
}

type Script struct {
	mu       sync.Mutex
	steps    []Step
	requests []model.Request
}

func New(steps ...Step) *Script {
	s := &Script{steps: append([]Step(nil), steps...)}
	for i := range s.steps {
		// Text fragments can split UTF-8 code points. JSON round-tripping would
		// replace their original bytes before the collector could join them.
		events := append([]model.Delta(nil), s.steps[i].Events...)
		for j := range events {
			events[j].Raw = append(json.RawMessage(nil), events[j].Raw...)
			if events[j].Usage != nil {
				u := *events[j].Usage
				if u.InputTokens != nil {
					n := *u.InputTokens
					u.InputTokens = &n
				}
				if u.OutputTokens != nil {
					n := *u.OutputTokens
					u.OutputTokens = &n
				}
				events[j].Usage = &u
			}
		}
		s.steps[i].Events = events
	}
	return s
}

func cloneRequest(r model.Request) model.Request {
	data, _ := json.Marshal(r)
	var copy model.Request
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (s *Script) Stream(ctx context.Context, request model.Request, emit func(model.Delta) error) error {
	s.mu.Lock()
	index := len(s.requests)
	s.requests = append(s.requests, cloneRequest(request))
	if index >= len(s.steps) {
		s.mu.Unlock()
		return fmt.Errorf("model fixture exhausted")
	}
	step := s.steps[index]
	s.mu.Unlock()
	if step.Check != nil {
		if err := step.Check(request); err != nil {
			return err
		}
	}
	for _, event := range step.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(event); err != nil {
			return err
		}
	}
	return step.Error
}

func (s *Script) Requests() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]model.Request, len(s.requests))
	for i, request := range s.requests {
		result[i] = cloneRequest(request)
	}
	return result
}

func Answer(category, text string) []model.Delta {
	return []model.Delta{{Kind: model.MessageStart, MessageID: "answer", Category: category}, {Kind: model.TextDelta, MessageID: "answer", Text: text}, {Kind: model.MessageEnd, MessageID: "answer"}, {Kind: model.ResponseEnd}}
}

func Call(id, name string, arguments ...string) []model.Delta {
	events := []model.Delta{{Kind: model.CallStart, CallID: id, Name: name}}
	for _, part := range arguments {
		events = append(events, model.Delta{Kind: model.ArgumentsDelta, CallID: id, Text: part})
	}
	return append(events, model.Delta{Kind: model.CallEnd, CallID: id}, model.Delta{Kind: model.ResponseEnd})
}
