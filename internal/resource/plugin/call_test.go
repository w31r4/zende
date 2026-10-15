package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPluginCallProcessHelper(t *testing.T) {
	if os.Getenv("ZENDE_PLUGIN_CALL_HELPER") != "1" {
		return
	}
	mode := os.Getenv("ZENDE_PLUGIN_CALL_MODE")
	var output sync.Mutex
	send := func(v any) {
		output.Lock()
		defer output.Unlock()
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			os.Exit(3)
		}
	}
	respond := func(id string, v any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v}) }
	cancels := map[string]chan struct{}{}
	var mu sync.Mutex
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	for scanner.Scan() {
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(4)
		}
		switch req.Method {
		case "plugin.initialize":
			var p InitializeParams
			_ = json.Unmarshal(req.Params, &p)
			respond(req.ID, InitializeResult{ProtocolVersion: 1, Plugin: p.Plugin, Capabilities: p.Capabilities})
		case "plugin.shutdown":
			respond(req.ID, nil)
			os.Exit(0)
		case "plugin.describe":
			d := Callable{Name: "echo", Description: "Echo a value", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"},"method":{"type":"string"}},"required":["value","method"]}`)}
			if mode == "catalog-invalid" {
				d.InputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"not-a-type"}}}`)
			}
			respond(req.ID, map[string]any{"tools": []Callable{d}, "commands": []Callable{d}})
		case "plugin.cancel":
			var p struct {
				RequestID   string `json:"request_id"`
				OperationID string `json:"operation_id"`
			}
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			if c := cancels[p.RequestID]; c != nil {
				close(c)
				delete(cancels, p.RequestID)
			}
			mu.Unlock()
		case "plugin.tool.call", "plugin.command.call":
			var p struct {
				OperationID   string          `json:"operation_id"`
				Name          string          `json:"name"`
				DefinitionRef string          `json:"definition_ref"`
				Scope         Scope           `json:"scope"`
				Arguments     json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil || p.Name != "echo" || !strings.HasPrefix(p.DefinitionRef, "plugin_call_") || !filepath.IsAbs(p.Scope.Cwd) {
				os.Exit(5)
			}
			var args struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(p.Arguments, &args)
			f, err := os.OpenFile("calls", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
			if err != nil {
				os.Exit(6)
			}
			fmt.Fprintln(f, p.OperationID)
			f.Close()
			cancel := make(chan struct{})
			mu.Lock()
			cancels[req.ID] = cancel
			mu.Unlock()
			id, method := req.ID, req.Method
			go func() {
				progress := func(n int64, value string) {
					send(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": Progress{RequestID: id, OperationID: p.OperationID, Sequence: n, Value: json.RawMessage(fmt.Sprintf(`{"state":%q}`, value))}})
				}
				out := Completion{OperationID: p.OperationID, State: "completed", Output: json.RawMessage(fmt.Sprintf(`{"value":%q,"method":%q}`, args.Value, method))}
				switch mode {
				case "progress-id-null":
					send(map[string]any{"jsonrpc": "2.0", "id": nil, "method": "plugin.progress", "params": Progress{RequestID: id, OperationID: p.OperationID, Sequence: 1, Value: json.RawMessage(`{}`)}})
					return
				case "progress-wrong-operation":
					send(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": Progress{RequestID: id, OperationID: "wrong", Sequence: 1, Value: json.RawMessage(`{}`)}})
					return
				case "progress-gap":
					progress(2, "running")
					return
				case "progress-overflow":
					for n := int64(1); n <= MaxQueuedProgress+4; n++ {
						progress(n, "running")
					}
					return
				case "progress-large":
					send(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": Progress{RequestID: id, OperationID: p.OperationID, Sequence: 1, Value: json.RawMessage(fmt.Sprintf(`%q`, strings.Repeat("x", MaxProgressBytes)))}})
					return
				}
				progress(1, "running")
				switch mode {
				case "hold", "cancel-completed":
					<-cancel
					progress(2, "cancelling")
					deadline := time.Now().Add(3 * time.Second)
					for {
						if _, err := os.Stat("release"); err == nil {
							break
						}
						if time.Now().After(deadline) {
							os.Exit(7)
						}
						time.Sleep(5 * time.Millisecond)
					}
					if mode == "hold" {
						out.State = "cancelled"
					}
				case "ignore":
					<-cancel
					time.Sleep(time.Minute)
					return
				case "exit":
					os.Exit(0)
				case "failed":
					out.State = "failed"
					out.Error = &CallFailure{Kind: "fixture_error", Message: "Known tool failure"}
				case "bad-output":
					out.Output = json.RawMessage(`{"value":7}`)
				case "bad-operation":
					out.OperationID = "other"
				case "bad-state":
					out.State = "running"
				case "rpc-error":
					send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": "private-diagnostic"}})
					return
				case "parallel":
					if args.Value == "slow" {
						time.Sleep(80 * time.Millisecond)
					}
				}
				respond(id, out)
				if mode == "complete-exit" {
					os.Exit(0)
				}
			}()
		default:
			os.Exit(8)
		}
	}
	os.Exit(0)
}

func callFixture(t *testing.T, mode string) (*Instance, *Catalog, string) {
	t.Helper()
	t.Setenv("ZENDE_PLUGIN_CALL_HELPER", "1")
	t.Setenv("ZENDE_PLUGIN_CALL_MODE", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := Manifest{SchemaVersion: 1, ID: "calls", Version: "1", ProtocolVersions: []int{1}, Command: binary, Args: []string{"-test.run=^TestPluginCallProcessHelper$"}, Capabilities: []string{"tools", "commands"}}
	i, err := Start(t.Context(), filepath.Join(dir, "plugin.json"), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { i.Close() })
	catalog, err := i.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i, catalog, dir
}

func prepareFixtureCall(t *testing.T, i *Instance, catalog *Catalog, kind, operation, value string) *Call {
	t.Helper()
	for _, d := range catalog.Definitions() {
		if d.Kind == kind {
			call, err := i.PrepareCall(d.Ref, operation, Scope{AgentID: "agent", TaskID: "task"}, json.RawMessage(fmt.Sprintf(`{"value":%q}`, value)))
			if err != nil {
				t.Fatal(err)
			}
			return call
		}
	}
	t.Fatal("missing callable", kind)
	return nil
}

func TestPluginToolsAndCommandsUseBoundCatalog(t *testing.T) {
	i, catalog, dir := callFixture(t, "ok")
	original := catalog.Definitions()
	mutated := catalog.Definitions()
	mutated[0].InputSchema[0] = 'x'
	if string(catalog.Definitions()[0].InputSchema) != string(original[0].InputSchema) {
		t.Fatal("mutable catalog")
	}
	again, err := i.Discover(t.Context())
	if err != nil || again.Version() != catalog.Version() {
		t.Fatal(again, err)
	}
	for _, kind := range []string{"tool", "command"} {
		call := prepareFixtureCall(t, i, catalog, kind, kind+"-op", "中文")
		arguments := call.Arguments()
		arguments[0] = 'x'
		if call.Arguments()[0] != '{' || call.Scope().Cwd != dir {
			t.Fatal("mutable arguments or missing effective scope")
		}
		if _, err := os.Stat(filepath.Join(dir, "calls")); kind == "tool" && !os.IsNotExist(err) {
			t.Fatal("preparation executed work")
		}
		var progress []Progress
		result, err := call.Invoke(t.Context(), func(p Progress) error { progress = append(progress, p); return nil })
		if err != nil || result.State != "completed" || result.InstanceID != i.Info().InstanceID || result.DefinitionRef != call.Definition().Ref || len(progress) != 1 || progress[0].RequestID != result.RequestID || progress[0].OperationID != result.OperationID {
			t.Fatal(result, progress, err)
		}
		var output struct {
			Value  string `json:"value"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(result.Completion.Output, &output)
		if output.Value != "中文" || output.Method != "plugin."+kind+".call" {
			t.Fatal(output)
		}
		_, err = call.Invoke(t.Context(), func(Progress) error { return nil })
		requireFailure(t, err, "operation_conflict")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Count(string(raw), "\n") != 2 {
		t.Fatal(string(raw), err)
	}
}

func TestPluginCallRejectsArgumentsBeforeDispatch(t *testing.T) {
	i, catalog, dir := callFixture(t, "ok")
	d := catalog.Definitions()[0]
	for _, raw := range []string{`{}`, `{"value":7}`, `{"value":"a","value":"b"}`, `{"value":"a","extra":1}`, `null`, `{"value":1e9999}`} {
		_, err := i.PrepareCall(d.Ref, "bad", Scope{}, json.RawMessage(raw))
		requireFailure(t, err, "invalid_arguments")
	}
	_, err := i.PrepareCall("missing", "bad", Scope{}, json.RawMessage(`{}`))
	requireFailure(t, err, "stale_reference")
	call := prepareFixtureCall(t, i, catalog, "tool", "cancelled-before-send", "value")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := call.Invoke(ctx, func(Progress) error { return nil })
	if !errors.Is(err, context.Canceled) || result.State != "not_dispatched" {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); !os.IsNotExist(err) {
		t.Fatal("invalid work dispatched", err)
	}
}

func TestPluginCancellationWaitsForRealOutcome(t *testing.T) {
	for _, mode := range []string{"hold", "cancel-completed"} {
		t.Run(mode, func(t *testing.T) {
			i, catalog, dir := callFixture(t, mode)
			call := prepareFixtureCall(t, i, catalog, "tool", "cancel", "value")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			progress := make(chan Progress, 2)
			done := make(chan CallResult, 1)
			go func() {
				result, err := call.Invoke(ctx, func(p Progress) error { progress <- p; return nil })
				if err != nil {
					t.Error(err)
				}
				done <- result
			}()
			select {
			case <-progress:
			case <-time.After(3 * time.Second):
				t.Fatal("missing running progress")
			}
			cancel()
			select {
			case p := <-progress:
				if !strings.Contains(string(p.Value), "cancelling") {
					t.Fatal(p)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing cancellation progress")
			}
			select {
			case result := <-done:
				t.Fatal("cancellation reported before confirmation", result)
			default:
			}
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				want := "cancelled"
				if mode == "cancel-completed" {
					want = "completed"
				}
				if result.State != want {
					t.Fatal(result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing terminal result")
			}
		})
	}
}

func TestPluginUnverifiedRepliesNeverBecomeSuccess(t *testing.T) {
	for _, mode := range []string{"exit", "rpc-error", "bad-output", "bad-operation", "bad-state", "progress-wrong-operation", "progress-gap", "progress-large", "progress-id-null"} {
		t.Run(mode, func(t *testing.T) {
			i, catalog, _ := callFixture(t, mode)
			call := prepareFixtureCall(t, i, catalog, "command", "unknown", "value")
			result, err := call.Invoke(t.Context(), func(Progress) error { return nil })
			requireFailure(t, err, "operation_unknown")
			if result.State != "unknown" || result.Completion != nil || strings.Contains(err.Error(), "private-diagnostic") {
				t.Fatal(result, err)
			}
			if strings.HasPrefix(mode, "bad-") || mode == "rpc-error" {
				if len(result.Evidence) == 0 {
					t.Fatal("lost invalid response evidence")
				}
			}
		})
	}
}

func TestPluginKnownFailureAndConcurrentCorrelation(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		i, catalog, _ := callFixture(t, "failed")
		result, err := prepareFixtureCall(t, i, catalog, "tool", "failed", "value").Invoke(t.Context(), func(Progress) error { return nil })
		if err != nil || result.State != "failed" || result.Completion.Error.Kind != "fixture_error" {
			t.Fatal(result, err)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		i, catalog, _ := callFixture(t, "parallel")
		calls := []*Call{prepareFixtureCall(t, i, catalog, "tool", "slow", "slow"), prepareFixtureCall(t, i, catalog, "command", "fast", "fast")}
		var wg sync.WaitGroup
		for _, call := range calls {
			wg.Go(func() {
				result, err := call.Invoke(t.Context(), func(p Progress) error {
					if p.OperationID != call.Binding().OperationID {
						t.Error("crossed progress")
					}
					return nil
				})
				if err != nil || result.OperationID != call.Binding().OperationID || !strings.Contains(string(result.Completion.Output), call.Binding().OperationID) {
					t.Error(result, err)
				}
			})
		}
		wg.Wait()
	})
}

func TestPluginCancellationTimeoutAndProgressFailure(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		i, catalog, _ := callFixture(t, "ignore")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result, err := prepareFixtureCall(t, i, catalog, "tool", "ignored", "value").Invoke(ctx, func(Progress) error { cancel(); return nil })
		requireFailure(t, err, "operation_unknown")
		if result.State != "unknown" || i.Info().State != "failed" {
			t.Fatal(result, i.Info())
		}
	})
	t.Run("observer", func(t *testing.T) {
		i, catalog, _ := callFixture(t, "ok")
		result, err := prepareFixtureCall(t, i, catalog, "tool", "observer", "value").Invoke(t.Context(), func(Progress) error { return errors.New("storage unavailable") })
		requireFailure(t, err, "operation_unknown")
		if result.State != "unknown" {
			t.Fatal(result)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		i, catalog, _ := callFixture(t, "progress-overflow")
		call := prepareFixtureCall(t, i, catalog, "tool", "overflow", "value")
		result, err := call.Invoke(t.Context(), func(Progress) error {
			select {
			case <-i.Done():
			case <-time.After(3 * time.Second):
				t.Error("progress overflow did not stop transport")
			}
			return nil
		})
		requireFailure(t, err, "operation_unknown")
		if result.State != "unknown" {
			t.Fatal(result)
		}
	})
}

func TestPluginConcurrentRequestLimitAndCancellationIsolation(t *testing.T) {
	i, catalog, dir := callFixture(t, "hold")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	running := make(chan Progress, MaxConcurrentRequests)
	done := make(chan CallResult, MaxConcurrentRequests)
	for n := range MaxConcurrentRequests {
		call := prepareFixtureCall(t, i, catalog, "tool", fmt.Sprintf("parallel-%d", n), "value")
		go func() {
			result, err := call.Invoke(ctx, func(p Progress) error {
				if p.Sequence == 1 {
					running <- p
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			done <- result
		}()
	}
	for range MaxConcurrentRequests {
		select {
		case <-running:
		case <-time.After(3 * time.Second):
			t.Fatal("request failed to start")
		}
	}
	result, err := prepareFixtureCall(t, i, catalog, "command", "over-limit", "value").Invoke(t.Context(), func(Progress) error { return nil })
	requireFailure(t, err, "resource_limit")
	if result.State != "not_dispatched" || i.Info().State != "ready" {
		t.Fatal(result, i.Info())
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cancel()
	for range MaxConcurrentRequests {
		select {
		case result := <-done:
			if result.State != "cancelled" {
				t.Fatal(result)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancellation blocked behind active requests")
		}
	}
	if i.Info().State != "ready" {
		t.Fatal("cancellation killed reusable plugin", i.Info())
	}
}

func TestPluginCompletionWinsCancellationRace(t *testing.T) {
	i, catalog, _ := callFixture(t, "ok")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	call := prepareFixtureCall(t, i, catalog, "tool", "race", "value")
	result, err := call.Invoke(ctx, func(Progress) error {
		// Let the protocol reader enqueue the already-sent terminal response.
		deadline := time.Now().Add(time.Second)
		for {
			i.mu.Lock()
			complete := false
			for _, p := range i.pending {
				if p.operation == "race" {
					complete = p.complete
				}
			}
			i.mu.Unlock()
			if complete {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("terminal reply not queued")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		return nil
	})
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
}

func TestPluginCompleteReplySurvivesProcessExit(t *testing.T) {
	i, catalog, _ := callFixture(t, "complete-exit")
	call := prepareFixtureCall(t, i, catalog, "command", "completed-before-exit", "value")
	result, err := call.Invoke(t.Context(), func(Progress) error {
		select {
		case <-i.Done():
		case <-time.After(3 * time.Second):
			return errors.New("fixture did not exit")
		}
		return nil
	})
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
}

func TestPluginInvalidCatalogCannotBecomeCallable(t *testing.T) {
	m, source := helperManifest(t, "good")
	m.Args = []string{"-test.run=^TestPluginCallProcessHelper$"}
	t.Setenv("ZENDE_PLUGIN_CALL_HELPER", "1")
	t.Setenv("ZENDE_PLUGIN_CALL_MODE", "catalog-invalid")
	i, err := Start(t.Context(), source, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	catalog, err := i.Discover(t.Context())
	requireFailure(t, err, "plugin_protocol")
	if catalog != nil || i.Info().State != "failed" {
		t.Fatal(catalog, i.Info())
	}
	_, err = i.PrepareCall("any", "not-dispatched", Scope{}, json.RawMessage(`{}`))
	requireFailure(t, err, "unavailable")
}
