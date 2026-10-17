package model

import (
	"encoding/json"

	"zende.local/harness/internal/protocol"
)

const PublicContext = "public"

// Version 1 pins the public-history conversion rules independently of the
// provider's snapshot version. The adapter remains unwrapped so plugin leases
// and native capabilities retain their normal runtime identities.
type contextBinding struct {
	Kind    string          `json:"kind"`
	Version int             `json:"version"`
	Binding json.RawMessage `json:"binding"`
}

func SplitContextBinding(raw json.RawMessage) (json.RawMessage, string, error) {
	bad := func() (json.RawMessage, string, error) {
		return nil, "", protocol.E("model_snapshot_invalid", "invalid or unsupported context conversion binding")
	}
	if len(raw) == 0 {
		return nil, "", nil
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if len(raw) > MaxBlockBytes || json.Unmarshal(raw, &header) != nil {
		return bad()
	}
	if header.Kind != "public-context" {
		return append(json.RawMessage(nil), raw...), "", nil
	}
	var saved contextBinding
	var innerHeader struct {
		Kind string `json:"kind"`
	}
	if protocol.StrictDecode(raw, &saved) != nil || saved.Version != 1 || len(saved.Binding) == 0 || string(saved.Binding) == "null" || json.Unmarshal(saved.Binding, &innerHeader) != nil || innerHeader.Kind == "public-context" {
		return bad()
	}
	return append(json.RawMessage(nil), saved.Binding...), PublicContext, nil
}

func WithContextMode(binding Binding, mode string) (Binding, error) {
	if mode != "" && mode != PublicContext {
		return Binding{}, protocol.E("invalid_params", "context mode must be strict or public")
	}
	inner, _, err := SplitContextBinding(binding.Snapshot)
	if err != nil {
		return Binding{}, err
	}
	if len(inner) == 0 {
		return Binding{}, protocol.E("model_snapshot_invalid", "context selection requires a saved provider binding")
	}
	binding.ContextMode, binding.Snapshot = mode, inner
	if mode == PublicContext {
		binding.Snapshot, err = json.Marshal(contextBinding{Kind: "public-context", Version: 1, Binding: inner})
		if err != nil || len(binding.Snapshot) > MaxBlockBytes {
			return Binding{}, protocol.E("model_snapshot_invalid", "context conversion binding exceeds the snapshot budget")
		}
	}
	return binding, nil
}

func ContextModeName(mode string) string {
	if mode == "" {
		return "strict"
	}
	return mode
}
