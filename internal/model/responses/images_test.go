package responses

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func TestImageTransportHasExplicitCapabilityAndByteBudget(t *testing.T) {
	r := baseRequest()
	r.Messages = []model.Message{{Role: "user", Content: []model.Block{{Type: "image", Attachment: "image"}}}}
	r.Images = map[string]model.Image{"image": {MediaType: "image/png", Data: []byte{1, 2, 3}}}
	credentials := 0
	a, err := New(Config{Endpoint: "https://fixture.invalid", Credential: func(context.Context) (string, error) { credentials++; return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Stream(context.Background(), r, func(model.Delta) error { return nil }); err == nil || credentials != 0 {
		t.Fatal("image capability checked after credentials/network", err)
	}
	encoded, err := buildRequest(r, "https://fixture.invalid", "")
	if err != nil || !strings.Contains(string(encoded), `"image_url":"data:image/png;base64,AQID"`) {
		t.Fatal(string(encoded), err)
	}
	ordinary, _ := json.Marshal(r)
	if strings.Contains(string(ordinary), "AQID") || strings.Contains(string(ordinary), "image/png") {
		t.Fatal("transient image bytes serialized as model history")
	}
	r.Images["image"] = model.Image{MediaType: "image/png", Data: make([]byte, protocol.MaxImageBytes)}
	r.Messages[0].Content = make([]model.Block, 4)
	for i := range r.Messages[0].Content {
		r.Messages[0].Content[i] = model.Block{Type: "image", Attachment: "image"}
	}
	if _, err := buildRequest(r, "https://fixture.invalid", ""); err == nil || err.(*protocol.Error).Data.Kind != "context_limit" {
		t.Fatal("duplicate wire occurrences bypassed byte budget", err)
	}
	r.Messages = r.Messages[:1]
	r.Messages[0].Content = r.Messages[0].Content[:1]
	for _, role := range []string{"system", "developer", "assistant", "tool"} {
		r.Messages[0].Role = role
		if _, err := buildRequest(r, "https://fixture.invalid", ""); err == nil {
			t.Fatal("unsupported image role silently accepted", role)
		}
	}
}
