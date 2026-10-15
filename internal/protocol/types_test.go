package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRequestRoundTripAndRetryIdentity(t *testing.T) {
	r, err := NewRequest("agent.create", "stable", map[string]any{"cwd": "/tmp", "input": "你好"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	var decoded Request
	if err := StrictDecode(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	f1, _ := r.Fingerprint()
	r.ID = NewID("rpc")
	r.Params = json.RawMessage(`{"input":"你好","cwd":"/tmp"}`)
	f2, _ := r.Fingerprint()
	if f1 != f2 {
		t.Fatal("transport retry changed mutation identity")
	}
	r.Method = "agent.submit"
	f3, _ := r.Fingerprint()
	if f1 == f3 {
		t.Fatal("different operation matched")
	}
}

func TestStrictDecode(t *testing.T) {
	for _, input := range []string{`{"extra":1}`, `{"id":"one","id":"two"}`, `{"id":1}`, `{} {}`, `{"params":{"a":1,"a":2}}`, `{`} {
		t.Run(input, func(t *testing.T) {
			var r Request
			if err := StrictDecode([]byte(input), &r); err == nil {
				t.Fatal("accepted invalid data")
			}
		})
	}
	r, _ := NewRequest("core.status", "", struct{}{})
	r.Version++
	var e *Error
	if err := r.Validate(); !errors.As(err, &e) || e.Data.Kind != "version_mismatch" {
		t.Fatal(err)
	}
}

func TestEventRoundTrip(t *testing.T) {
	e := Event{Version: Version, AgentID: "a", Seq: 2, Kind: "task.accepted", TaskID: "t", Payload: json.RawMessage(`{"state":"accepted"}`)}
	b, _ := json.Marshal(e)
	var out Event
	if err := StrictDecode(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Seq != 2 || out.Kind != e.Kind || out.AgentID != "a" {
		t.Fatal(out)
	}
}
