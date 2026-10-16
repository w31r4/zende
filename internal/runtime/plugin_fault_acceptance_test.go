package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zende.local/harness/internal/model/fixture"
	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/store"
)

func TestPluginEffectSurvivesExitWithoutReplay(t *testing.T) {
	for _, kind := range []string{"tool", "command"} {
		t.Run(kind, func(t *testing.T) {
			effect := filepath.Join(t.TempDir(), "effect.jsonl")
			t.Setenv("ZENDE_TEST_PLUGIN_EFFECT", effect)
			c, target, _, source := registeredPlugin(t, "call-effect-exit")
			_, inv, release := prepareRuntimePlugin(t, c, target, "original-effect", kind)
			release()
			first, err := c.startPluginCall(t.Context(), inv.Request, inv.Scope)
			if err != nil {
				t.Fatal(err)
			}
			unknown := waitPluginReceipt(t, c, first.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "unknown" })
			if unknown.ProgressSeq != 1 || unknown.Binding != first.Binding || unknown.Error == nil || unknown.Error.Data.Kind != "operation_unknown" {
				t.Fatal("exit erased progress or fabricated completion", unknown)
			}
			if completion, err := c.Store.PluginCompletion(t.Context(), first.PluginCallTarget); completion != nil || !isFailure(err, "operation_unknown") {
				t.Fatal("unknown effect exposed a verified result", completion, err)
			}
			checkEffect := func() {
				t.Helper()
				raw, err := os.ReadFile(effect)
				var record struct {
					OperationID string `json:"operation_id"`
				}
				if err != nil || bytes.Count(raw, []byte{'\n'}) != 1 || json.Unmarshal(raw, &record) != nil || record.OperationID != "original-effect" {
					t.Fatal("effect missing or repeated", string(raw), err)
				}
			}
			checkEffect()
			t.Setenv("ZENDE_TEST_PLUGIN_MODE", "")
			// Another Agent on this same Core uses a separate healthy instance.
			other := createWork(t, c, "")
			register, _ := protocol.NewRequest("resource.register", "other-plugin", protocol.RegisterResource{AgentID: other.AgentID, Kind: "plugin", Path: source})
			v, err := c.Handle(t.Context(), register)
			if err != nil {
				t.Fatal(err)
			}
			otherTarget := protocol.ResourceTarget{AgentID: other.AgentID, ID: v.(protocol.Accepted).Resource.ID}
			_, next, releaseOther := prepareRuntimePlugin(t, c, otherTarget, "healthy-call", kind)
			releaseOther()
			healthy, err := c.startPluginCall(t.Context(), next.Request, next.Scope)
			if err != nil {
				t.Fatal(err)
			}
			waitPluginReceipt(t, c, healthy.PluginCallTarget, func(r store.PluginReceipt) bool { return r.State == "completed" })
			// Also run a real scheduler Task to final, not just a transport call.
			c.Models = &savedFixture{adapter: fixture.New(fixture.Step{Events: fixture.Answer("final", "unrelated work completed")})}
			work := submitWork(t, c, other.AgentID, "continue despite the other plugin's exit")
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			go func() { runErr = c.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			waitTask(t, c, work, "completed")
			cancel()
			<-done
			if runErr != nil {
				t.Fatal(runErr)
			}
			c.CloseResources()
			// Reopen the actual database with a new Core owner. Explicit refresh
			// prepares a new process but must never authorize the unknown effect.
			dbPath := filepath.Join(filepath.Dir(source), "state.db")
			if err := c.Store.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			replacement := New(s, c.Config)
			defer replacement.CloseResources()
			if err := s.InterruptPluginCalls(t.Context(), replacement.InstanceID); err != nil {
				t.Fatal(err)
			}
			refresh, _ := protocol.NewRequest("resource.refresh", "explicit-refresh", target)
			if _, err := replacement.Handle(t.Context(), refresh); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				retry := inv.Request
				retry.ID = protocol.NewID("rpc")
				r, err := replacement.startPluginCall(t.Context(), retry, inv.Scope)
				if err != nil || r.State != "unknown" || r.Binding != first.Binding || r.ProgressSeq != 1 {
					t.Fatal("retry lost original outcome", r, err)
				}
			}
			changed := inv.Request
			changed.OperationID = "different-id"
			if _, err := replacement.startPluginCall(t.Context(), changed, inv.Scope); !isFailure(err, "operation_unknown") {
				t.Fatal("refresh/new ID bypassed unknown effect", err)
			}
			// Retained progress remains readable without contacting the instance.
			progress, err := s.PluginProgress(t.Context(), first.PluginCallTarget, 0, 16)
			if err != nil || len(progress.Items) != 1 {
				t.Fatal(progress, err)
			}
			checkEffect()
			audit, err := os.ReadFile(os.Getenv("ZENDE_TEST_PLUGIN_AUDIT"))
			if err != nil || bytes.Count(audit, []byte(`"method":"plugin.`+kind+`.call"`)) != 2 {
				t.Fatal("effect dispatched again", string(audit), err)
			}
		})
	}
}
