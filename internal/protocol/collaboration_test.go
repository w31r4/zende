package protocol

import "testing"

func TestCollaborationEffectIdentityExcludesRetryCallerOnly(t *testing.T) {
	for _, method := range []string{"agent.spawn", "agent.submit"} {
		first, _ := NewRequest(method, "stable", map[string]any{"input": "work", "caller": Caller{AgentID: "a", TaskID: "t", OperationID: "first"}})
		retry, _ := NewRequest(method, "stable", map[string]any{"input": "work", "caller": Caller{AgentID: "a", TaskID: "t", OperationID: "second"}})
		human, _ := NewRequest(method, "stable", map[string]any{"input": "work"})
		before, _ := first.EffectFingerprint()
		after, _ := retry.EffectFingerprint()
		without, _ := human.EffectFingerprint()
		if before != after || before != without {
			t.Fatal("caller changed effect identity", method)
		}
		a, _ := first.Fingerprint()
		b, _ := retry.Fingerprint()
		if a == b {
			t.Fatal("automatic caller-local identities collided")
		}
		changed, _ := NewRequest(method, "stable", map[string]any{"input": "different"})
		change, _ := changed.EffectFingerprint()
		if before == change {
			t.Fatal("changed work accepted as retry")
		}
		first.Method, retry.Method = "process.input", "process.input"
		a, _ = first.EffectFingerprint()
		b, _ = retry.EffectFingerprint()
		if a == b {
			t.Fatal("unrelated fingerprints changed")
		}
	}
}
