package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
	"zende.local/harness/internal/resource/plugin"
	"zende.local/harness/internal/resource/plugin/fixture"
)

func TestStorePluginProcessHelper(t *testing.T) { fixture.RunFromEnv() }

type pluginJournalFixture struct {
	s          *Store
	path       string
	agent      string
	source     string
	manifest   plugin.Manifest
	instance   *plugin.Instance
	pkg        resource.Package
	generation int64
}

func newPluginJournal(t *testing.T, mode string) *pluginJournalFixture {
	t.Helper()
	s, path := newStore(t)
	p := protocol.CreateAgent{Cwd: filepath.Dir(path)}
	req, _ := protocol.NewRequest("agent.create", "create", p)
	a, err := s.Create(t.Context(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENDE_TEST_PLUGIN", "1")
	t.Setenv("ZENDE_TEST_PLUGIN_MODE", mode)
	t.Setenv("ZENDE_TEST_PLUGIN_REWRITE", "")
	t.Setenv("ZENDE_TEST_PLUGIN_AUDIT", filepath.Join(filepath.Dir(path), "audit.jsonl"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &pluginJournalFixture{s: s, path: path, agent: a.AgentID, source: filepath.Join(filepath.Dir(path), "plugin.json"), manifest: plugin.Manifest{SchemaVersion: 1, ID: "fixture", Version: "1", Command: exe, Args: []string{"-test.run=^TestStorePluginProcessHelper$"}, ProtocolVersions: []int{1}, Capabilities: []string{"tools", "commands"}}}
	f.publish(t, "1")
	return f
}

func (f *pluginJournalFixture) publish(t *testing.T, version string) {
	t.Helper()
	f.manifest.Version = version
	raw, _ := json.Marshal(f.manifest)
	if err := os.WriteFile(f.source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	pkg, instance, err := resource.PreparePluginPackage(t.Context(), f.source, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	p := protocol.RegisterResource{AgentID: f.agent, Kind: "plugin", Path: f.source}
	req, _ := protocol.NewRequest("resource.register", "publish-"+version, p)
	a, err := f.s.PublishResource(t.Context(), req, f.agent, pkg, f.generation)
	if err != nil {
		t.Fatal(err)
	}
	f.pkg, f.instance, f.generation = pkg, instance, a.ResourceGeneration
}

func (f *pluginJournalFixture) prepare(t *testing.T, operation, kind string) (*plugin.Call, PluginInvocation, PluginCallTarget) {
	t.Helper()
	var ref string
	for _, d := range f.pkg.Plugin.Definitions {
		if d.Kind == kind {
			ref = d.Ref
		}
	}
	args := json.RawMessage(`{"text":"private-result-中文"}`)
	call, err := f.instance.PrepareCall(ref, operation, plugin.Scope{AgentID: f.agent, Cwd: filepath.Dir(f.path)}, args)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := protocol.NewRequest("plugin."+kind+".call", operation, PluginCallInput{AgentID: f.agent, ResourceID: f.pkg.Info.ID, DefinitionRef: ref, Arguments: args})
	inv := PluginInvocation{Request: req, Binding: call.Binding(), Scope: call.Scope(), Definition: call.Definition(), ResourceVersion: f.pkg.Info.Version, Generation: f.generation}
	return call, inv, PluginCallTarget{AgentID: f.agent, OperationID: operation}
}

func beginPlugin(t *testing.T, f *pluginJournalFixture, inv PluginInvocation) {
	t.Helper()
	r, fresh, err := f.s.BeginPluginCall(t.Context(), inv, "owner")
	if err != nil || !fresh || r.Receipt.State != "dispatched" {
		t.Fatal(r, fresh, err)
	}
}

func wantPluginError(t *testing.T, err error, kind string) {
	t.Helper()
	var e *protocol.Error
	if !errors.As(err, &e) || e.Data.Kind != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func TestPluginJournalRealCallsAndRetainedResults(t *testing.T) {
	for _, tc := range []struct{ kind, mode, state string }{{"tool", "", "completed"}, {"command", "", "completed"}, {"tool", "call-failed", "failed"}, {"command", "call-cancel", "cancelled"}, {"tool", "call-exit", "unknown"}} {
		t.Run(tc.kind+"-"+tc.state, func(t *testing.T) {
			f := newPluginJournal(t, tc.mode)
			call, inv, target := f.prepare(t, "call", tc.kind)
			beginPlugin(t, f, inv)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, invokeErr := call.Invoke(ctx, func(p plugin.Progress) error {
				if err := f.s.AppendPluginProgress(t.Context(), target, "owner", p); err != nil {
					return err
				}
				if tc.mode == "call-cancel" && p.Sequence == 1 {
					r, err := f.s.MarkPluginCancelling(t.Context(), target, "owner")
					if err != nil {
						return err
					}
					if r.Receipt.State != "cancelling" {
						return errors.New("cancel fabricated terminal state")
					}
					cancel()
				}
				return nil
			})
			if result.State != tc.state || (invokeErr != nil) != (tc.state == "unknown") {
				t.Fatal(result, invokeErr)
			}
			r, err := f.s.FinishPluginCall(t.Context(), target, "owner", result)
			if err != nil || r.Receipt.State != tc.state {
				t.Fatal(r, err)
			}
			if again, err := f.s.FinishPluginCall(t.Context(), target, "owner", result); err != nil || !reflect.DeepEqual(r, again) {
				t.Fatal(again, err)
			}
			// Accepted identity wins even when a caller supplies another live binding.
			inv.Request.ID = "retry-transport"
			inv.Binding.InstanceID = "different-process"
			again, fresh, err := f.s.BeginPluginCall(t.Context(), inv, "replacement")
			if err != nil || fresh || !reflect.DeepEqual(r, again) {
				t.Fatal(again, fresh, err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.InterruptPluginCalls(t.Context(), "replacement"); err != nil {
				t.Fatal(err)
			}
			prior, err := reopened.PriorPluginCall(t.Context(), inv.Request)
			if err != nil || !reflect.DeepEqual(prior, r) {
				t.Fatal(prior, err)
			}
			progress, err := reopened.PluginProgress(t.Context(), target, 0, 1)
			wantCount := int64(1)
			if tc.mode == "call-cancel" {
				wantCount = 2
			}
			if err != nil || len(progress.Items) != 1 || progress.More != (wantCount == 2) {
				t.Fatal(progress, err)
			}
			if progress.More {
				next, err := reopened.PluginProgress(t.Context(), target, progress.Next, 1)
				if err != nil || next.Next != 2 || next.More {
					t.Fatal(next, err)
				}
			}
			// UTF-8 byte pages reconstruct the retained result without invoking again.
			var retained strings.Builder
			offset := 0
			for {
				page, err := reopened.ReadPluginResult(t.Context(), target, offset, 7)
				if err != nil || !utf8.ValidString(page.Text) || page.SHA256 != r.Receipt.ResultSHA256 {
					t.Fatal(page, err)
				}
				retained.WriteString(page.Text)
				if !page.More {
					break
				}
				if page.Next <= offset {
					t.Fatal("reader did not advance")
				}
				offset = page.Next
			}
			raw, _ := json.Marshal(result)
			if retained.String() != string(raw) {
				t.Fatal("changed result bytes")
			}
			events, err := reopened.Events(t.Context(), protocol.Cursor{AgentID: f.agent})
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(events)
			for _, secret := range []string{"private-result", "private-progress", "private-failure"} {
				if strings.Contains(string(b), secret) {
					t.Fatal("private plugin content leaked to events")
				}
			}
		})
	}
}

func TestPluginJournalAtomicAcceptanceAndConcurrentDedup(t *testing.T) {
	f := newPluginJournal(t, "")
	_, inv, target := f.prepare(t, "call", "tool")
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_plugin_accept BEFORE INSERT ON operations WHEN NEW.id='call' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if r, fresh, err := f.s.BeginPluginCall(t.Context(), inv, "owner"); err == nil || fresh || r != nil {
		t.Fatal(r, fresh, err)
	}
	if r, err := f.s.PluginCall(t.Context(), target); err != nil || r != nil {
		t.Fatal("dispatch escaped failed acceptance", r, err)
	}
	var count int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM events WHERE kind='plugin.dispatched'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER fail_plugin_accept`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	grants := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fresh, err := f.s.BeginPluginCall(t.Context(), inv, "owner")
			if err != nil {
				t.Error(err)
			}
			grants <- fresh
		}()
	}
	wg.Wait()
	close(grants)
	count = 0
	for fresh := range grants {
		if fresh {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dispatch grants=%d", count)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM events WHERE kind='plugin.dispatched'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	p, _ := pluginInput(inv.Request)
	p.Arguments = json.RawMessage(`{"text":"different"}`)
	inv.Request.Params, _ = json.Marshal(p)
	_, err := f.s.PriorPluginCall(t.Context(), inv.Request)
	wantPluginError(t, err, "operation_conflict")
}

func TestPluginJournalPublicationScopeAndArguments(t *testing.T) {
	f := newPluginJournal(t, "")
	_, original, _ := f.prepare(t, "call", "tool")
	for name, change := range map[string]func(*PluginInvocation){
		"release":    func(i *PluginInvocation) { i.Binding.PluginVersion = "other" },
		"manifest":   func(i *PluginInvocation) { i.Binding.ManifestVersion = "other" },
		"instance":   func(i *PluginInvocation) { i.Binding.InstanceID = "" },
		"catalog":    func(i *PluginInvocation) { i.Binding.CatalogVersion = "other" },
		"generation": func(i *PluginInvocation) { i.Generation++ },
		"definition": func(i *PluginInvocation) { i.Definition.Description = "changed" },
		"scope":      func(i *PluginInvocation) { i.Scope.AgentID = "other" },
		"cwd":        func(i *PluginInvocation) { i.Scope.Cwd = "relative" },
		"kind":       func(i *PluginInvocation) { i.Request.Method = "plugin.command.call" },
		"arguments": func(i *PluginInvocation) {
			p, _ := pluginInput(i.Request)
			p.Arguments = json.RawMessage(`{"text":false}`)
			i.Request.Params, _ = json.Marshal(p)
		},
		"inactive-task": func(i *PluginInvocation) {
			p, _ := pluginInput(i.Request)
			p.TaskID = "absent"
			i.Scope.TaskID = p.TaskID
			i.Request.Params, _ = json.Marshal(p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := original
			change(&i)
			if _, fresh, err := f.s.BeginPluginCall(t.Context(), i, "owner"); err == nil || fresh {
				t.Fatal("accepted invalid dispatch", err)
			}
			if _, found, err := f.s.AcceptedRequest(t.Context(), i.Request); err != nil || found {
				t.Fatal("invalid dispatch reserved operation", found, err)
			}
		})
	}
	beginPlugin(t, f, original)
}

func TestPluginJournalPinsResultSchemaAcrossRefresh(t *testing.T) {
	f := newPluginJournal(t, "")
	call, inv, target := f.prepare(t, "call", "tool")
	beginPlugin(t, f, inv)
	f.publish(t, "2") // Old instance is retained by this test's owner.
	result, err := call.Invoke(t.Context(), func(p plugin.Progress) error { return f.s.AppendPluginProgress(t.Context(), target, "owner", p) })
	if err != nil {
		t.Fatal(err)
	}
	bad := result
	completion := *result.Completion
	completion.Output = json.RawMessage(`{"text":123}`)
	bad.Completion = &completion
	bad.Evidence, _ = json.Marshal(completion)
	if _, err := f.s.FinishPluginCall(t.Context(), target, "owner", bad); err == nil {
		t.Fatal("schema-invalid result accepted")
	}
	bad = result
	bad.CallBinding.CatalogVersion = f.pkg.Plugin.CatalogVersion
	if _, err := f.s.FinishPluginCall(t.Context(), target, "owner", bad); err == nil {
		t.Fatal("current binding replaced original")
	}
	r, err := f.s.FinishPluginCall(t.Context(), target, "owner", result)
	if err != nil || r.Receipt.Generation != inv.Generation || r.Receipt.ResourceVersion != inv.ResourceVersion {
		t.Fatal(r, err)
	}
}

func TestPluginJournalRequiresActiveTask(t *testing.T) {
	f := newPluginJournal(t, "")
	p := protocol.Submit{AgentID: f.agent, Input: "work"}
	req, _ := protocol.NewRequest("agent.submit", "task", p)
	a, err := f.s.Submit(t.Context(), req, p, 8)
	if err != nil {
		t.Fatal(err)
	}
	_, inv, _ := f.prepare(t, "call", "tool")
	input, _ := pluginInput(inv.Request)
	input.TaskID, inv.Scope.TaskID = a.TaskID, a.TaskID
	inv.Request.Params, _ = json.Marshal(input)
	if _, fresh, err := f.s.BeginPluginCall(t.Context(), inv, "owner"); err == nil || fresh {
		t.Fatal("queued task dispatched a plugin call", fresh, err)
	}
	if w, err := f.s.ClaimNext(t.Context(), f.agent); err != nil || w == nil {
		t.Fatal(w, err)
	}
	beginPlugin(t, f, inv)
	if _, err := f.s.db.Exec(`UPDATE agents SET paused=1 WHERE id=?`, f.agent); err != nil {
		t.Fatal(err)
	}
	_, inv, _ = f.prepare(t, "paused-call", "tool")
	input, _ = pluginInput(inv.Request)
	input.TaskID, inv.Scope.TaskID = a.TaskID, a.TaskID
	inv.Request.Params, _ = json.Marshal(input)
	_, _, err = f.s.BeginPluginCall(t.Context(), inv, "owner")
	wantPluginError(t, err, "task_cancelled")
}
