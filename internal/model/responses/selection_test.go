package responses

import (
	"context"
	"encoding/json"
	"testing"

	"zende.local/harness/internal/model"
)

func TestSelectionPreflightAllowsPendingCallsButRequestDoesNot(t *testing.T) {
	a, err := New(Config{Endpoint: "https://test.invalid/v1", Credential: func(context.Context) (string, error) {
		t.Error("preflight resolved credentials")
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := baseRequest()
	req.Messages = append(req.Messages, model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "pending", Name: "act", Arguments: json.RawMessage(`{}`)}}})
	if err := a.ValidateContext(req); err != nil {
		t.Fatal("selection blocked on its own CLI call", err)
	}
	if err := a.ValidateRequest(req); err == nil {
		t.Fatal("dispatched incomplete tool exchange")
	}
	req.Messages = append(req.Messages, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "pending", Text: "done"}}})
	if err := a.ValidateRequest(req); err != nil {
		t.Fatal(err)
	}
	// Context preflight must still reject orphan results and private mismatches.
	req.Messages = append(req.Messages, req.Messages[len(req.Messages)-1])
	if err := a.ValidateContext(req); err == nil {
		t.Fatal("accepted duplicate result")
	}
	req = baseRequest()
	req.Messages = append(req.Messages, model.Message{Role: "assistant", Protocol: "different", Opaque: json.RawMessage(`{}`)})
	if err := a.ValidateContext(req); err == nil {
		t.Fatal("accepted incompatible private context")
	}
	req = baseRequest()
	req.Messages = append(req.Messages, model.Message{Role: "user", Content: []model.Block{{Type: "image", Attachment: "image"}}})
	if err := a.ValidateContext(req); err == nil {
		t.Fatal("accepted unsupported image")
	}
}
