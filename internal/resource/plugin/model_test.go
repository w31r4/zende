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

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func TestPluginModelProcessHelper(t *testing.T) {
	if os.Getenv("ZENDE_PLUGIN_MODEL_HELPER") != "1" {
		return
	}
	mode := os.Getenv("ZENDE_PLUGIN_MODEL_MODE")
	var output, mu sync.Mutex
	send := func(v any) {
		output.Lock()
		defer output.Unlock()
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			os.Exit(3)
		}
	}
	respond := func(id string, v any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v}) }
	cancels := map[string]chan struct{}{}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), MaxModelRequestBytes+1)
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
			if p.Limits.ModelRequestBytes != MaxModelRequestBytes || p.Limits.QueuedModelBytes != MaxQueuedModelBytes {
				os.Exit(5)
			}
			respond(req.ID, InitializeResult{ProtocolVersion: 1, Plugin: p.Plugin, Capabilities: p.Capabilities})
		case "plugin.describe":
			respond(req.ID, catalogReply{Providers: []Provider{{Name: "models", Description: "Fixture models", Models: []protocol.ModelInfo{{ID: "fixture", ContextWindow: 32000, MaxOutput: 4096, NativeCategories: true, Images: true, Efforts: []string{"low", "high"}}}}}})
		case "plugin.shutdown":
			respond(req.ID, nil)
			os.Exit(0)
		case "plugin.cancel":
			var p struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			if ch := cancels[p.RequestID]; ch != nil {
				close(ch)
				delete(cancels, p.RequestID)
			}
			mu.Unlock()
		case "plugin.model.stream":
			var p ModelParams
			if protocol.StrictDecode(req.Params, &p) != nil || p.OperationID != p.Request.ID || p.Name != "models" || p.Effort != "high" || p.Request.Model != "fixture" || !strings.HasPrefix(p.DefinitionRef, "plugin_provider_") || !filepath.IsAbs(p.Scope.Cwd) || p.Protocol == "" {
				os.Exit(6)
			}
			if err := os.WriteFile("dispatched-"+p.OperationID, []byte(p.DefinitionRef), 0600); err != nil {
				os.Exit(7)
			}
			id := req.ID
			cancel := make(chan struct{})
			mu.Lock()
			cancels[id] = cancel
			mu.Unlock()
			go func() {
				var seq int64
				emit := func(d model.Delta) {
					seq++
					operation, sequence := p.OperationID, seq
					if mode == "wrong-operation" {
						operation = "wrong"
					}
					if mode == "gap" {
						sequence++
					}
					send(map[string]any{"jsonrpc": "2.0", "method": "plugin.model.delta", "params": ModelDelta{RequestID: id, OperationID: operation, Sequence: sequence, Delta: d}})
				}
				finish := func(state string) {
					c := ModelCompletion{OperationID: p.OperationID, State: state}
					if state == "failed" {
						c.Error = &CallFailure{Kind: "fixture_error", Message: "private-diagnostic"}
					}
					if mode == "wrong-completion" {
						c.OperationID = "wrong"
					}
					respond(id, c)
				}
				if mode == "progress-channel" {
					send(map[string]any{"jsonrpc": "2.0", "method": "plugin.progress", "params": Progress{RequestID: id, OperationID: p.OperationID, Sequence: 1, Value: json.RawMessage(`{}`)}})
					return
				}
				if mode == "failed" {
					finish("failed")
					return
				}
				if mode == "rpc-error" {
					send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": "private-diagnostic"}})
					return
				}
				if mode == "tools" && len(p.Request.Messages) == 1 || mode == "partial-call" {
					emit(model.Delta{Kind: model.CallStart, CallID: "call-1", Name: "echo"})
					emit(model.Delta{Kind: model.ArgumentsDelta, CallID: "call-1", Text: `{"value":`})
					if mode == "partial-call" {
						os.Exit(0)
					}
					emit(model.Delta{Kind: model.ArgumentsDelta, CallID: "call-1", Text: `"插件"}`})
					emit(model.Delta{Kind: model.CallEnd, CallID: "call-1"})
					emit(model.Delta{Kind: model.ResponseEnd})
					finish("completed")
					return
				}
				mid := "m-" + p.OperationID
				emit(model.Delta{Kind: model.MessageStart, MessageID: mid, Category: "final"})
				if strings.Contains(p.OperationID, "hold") {
					<-cancel
					if mode == "ignore-cancel" {
						select {}
					}
					if mode == "confirm-cancel" {
						_ = os.WriteFile("cancel-received", nil, 0600)
						for {
							if _, err := os.Stat("release"); err == nil {
								break
							}
							time.Sleep(time.Millisecond)
						}
					}
					finish("cancelled")
					return
				}
				if mode == "overflow" {
					for range MaxQueuedProgress + 10 {
						emit(model.Delta{Kind: model.TextDelta, MessageID: mid, Text: strings.Repeat("x", 1024)})
					}
					return
				}
				text := p.OperationID
				if mode == "images" {
					if len(p.Images) != 3 {
						os.Exit(8)
					}
					for _, image := range p.Images {
						if image.MediaType != "image/png" || len(image.Data) != protocol.MaxImageBytes || image.Data[0] != 42 {
							os.Exit(9)
						}
					}
					text = "three images"
				}
				emit(model.Delta{Kind: model.TextDelta, MessageID: mid, Text: text})
				if mode == "opaque" || mode == "foreign-opaque" {
					pr := p.Protocol
					if mode == "foreign-opaque" {
						pr = "responses"
					}
					emit(model.Delta{Kind: model.ProtocolData, MessageID: mid, Protocol: pr, Raw: json.RawMessage(`{"continuation":"opaque"}`)})
				}
				emit(model.Delta{Kind: model.MessageEnd, MessageID: mid})
				if mode != "missing-end" {
					emit(model.Delta{Kind: model.ResponseEnd})
				}
				if mode == "exit-before-reply" {
					os.Exit(0)
				}
				finish("completed")
				if mode == "complete-exit" {
					os.Exit(0)
				}
			}()
		default:
			os.Exit(10)
		}
	}
	os.Exit(0)
}

func modelFixture(t *testing.T, mode string) (*ModelAdapter, string) {
	t.Helper()
	t.Setenv("ZENDE_PLUGIN_MODEL_HELPER", "1")
	t.Setenv("ZENDE_PLUGIN_MODEL_MODE", mode)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := Manifest{SchemaVersion: 1, ID: "models", Version: "1", ProtocolVersions: []int{1}, Command: binary, Args: []string{"-test.run=^TestPluginModelProcessHelper$"}, Capabilities: []string{"providers"}}
	i, err := Start(t.Context(), filepath.Join(dir, "plugin.json"), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { i.Close() })
	c, err := i.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, err := i.Model(c.Providers()[0].Ref, "fixture", "high", Scope{AgentID: "agent", TaskID: "task"})
	if err != nil {
		t.Fatal(err)
	}
	return a, dir
}

func modelRequest(id string) model.Request {
	return model.Request{ID: id, Model: "fixture", System: "Be concise", MaxOutputTokens: 1024, Messages: []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "hello"}}}}, Tools: []model.Tool{{Name: "echo", Description: "Echo a value", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
}

func TestPluginModelTwoRoundToolAndPrivateState(t *testing.T) {
	for _, mode := range []string{"tools", "opaque"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := modelFixture(t, mode)
			r := modelRequest("first")
			var c model.Collector
			if err := a.Stream(t.Context(), r, c.Add); err != nil {
				t.Fatal(err)
			}
			response, err := c.Response()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "tools" {
				if len(response.Calls) != 1 || string(response.Calls[0].Arguments) != `{"value":"插件"}` {
					t.Fatal(response)
				}
				r.Messages = append(r.Messages, response.Messages...)
				r.Messages = append(r.Messages, model.Message{Role: "tool", Content: []model.Block{{Type: "text", CallID: "call-1", Text: "tool failed", IsError: true}}})
			} else {
				if response.Messages[0].Protocol != a.Protocol() || len(response.Messages[0].Opaque) == 0 {
					t.Fatal(response)
				}
				r.Messages = append(r.Messages, response.Messages...)
			}
			r.ID = "second"
			var next model.Collector
			if err := a.Stream(t.Context(), r, next.Add); err != nil {
				t.Fatal(err)
			}
			response, err = next.Response()
			if err != nil {
				t.Fatal(err)
			}
			text, final, err := response.Final()
			if err != nil || !final || text != "second" {
				t.Fatal(text, final, err)
			}
			if response.Usage != nil {
				t.Fatal("fabricated usage")
			}
		})
	}
}

func TestPluginModelPreflightNeverDispatchesInvalidContext(t *testing.T) {
	a, dir := modelFixture(t, "ok")
	for _, mutate := range []func(*model.Request){
		func(r *model.Request) { r.Model = "missing" },
		func(r *model.Request) { r.MaxOutputTokens = 9000 },
		func(r *model.Request) { r.ID = "" },
		func(r *model.Request) { r.Messages[0].Role = "unknown" },
		func(r *model.Request) { r.Messages[0].Protocol = "responses" },
		func(r *model.Request) { r.Messages[0].Content[0].Type = "unknown" },
		func(r *model.Request) {
			r.Images = map[string]model.Image{"unused": {MediaType: "image/png", Data: []byte{1}}}
		},
		func(r *model.Request) { r.Tools = append(r.Tools, r.Tools[0]) },
		func(r *model.Request) {
			r.Messages = append(r.Messages, model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "c", Name: "echo", Arguments: json.RawMessage(`{}`)}}})
		},
	} {
		r := modelRequest("invalid")
		mutate(&r)
		if err := a.Stream(t.Context(), r, func(model.Delta) error { t.Error("invalid request emitted output"); return nil }); err == nil {
			t.Fatal("invalid request accepted", r)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "dispatched-invalid")); !os.IsNotExist(err) {
		t.Fatal("invalid request dispatched")
	}
	r := modelRequest("pending")
	r.Messages = append(r.Messages, model.Message{Role: "assistant", Content: []model.Block{{Type: "tool_call", CallID: "c", Name: "echo", Arguments: json.RawMessage(`{}`)}}})
	if err := a.ValidateContext(r); err != nil {
		t.Fatal(err)
	}
	_, err := a.instance.Model(a.binding.DefinitionRef, "fixture", "unsupported", Scope{})
	requireFailure(t, err, "unsupported_capability")
	_, err = a.instance.Model("missing", "fixture", "high", Scope{})
	requireFailure(t, err, "stale_reference")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Stream(ctx, modelRequest("cancelled"), func(model.Delta) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dispatched-cancelled")); !os.IsNotExist(err) {
		t.Fatal("pre-cancelled request dispatched")
	}
}

func TestPluginModelResolvedImagesExceedOrdinaryFrame(t *testing.T) {
	a, _ := modelFixture(t, "images")
	r := modelRequest("images")
	r.Images = map[string]model.Image{}
	for n := range 3 {
		id := fmt.Sprintf("image-%d", n)
		data := make([]byte, protocol.MaxImageBytes)
		data[0] = 42
		r.Images[id] = model.Image{MediaType: "image/png", Data: data}
		r.Messages[0].Content = append(r.Messages[0].Content, model.Block{Type: "image", Attachment: id})
	}
	var c model.Collector
	if err := a.Stream(t.Context(), r, c.Add); err != nil {
		t.Fatal(err)
	}
	response, err := c.Response()
	if err != nil {
		t.Fatal(err)
	}
	text, final, err := response.Final()
	if text != "three images" || !final || err != nil {
		t.Fatal(text, final, err)
	}
}

func TestPluginModelRejectsBrokenStreams(t *testing.T) {
	for _, mode := range []string{"progress-channel", "wrong-operation", "gap", "partial-call", "missing-end", "wrong-completion", "foreign-opaque", "exit-before-reply", "rpc-error", "failed"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := modelFixture(t, mode)
			var c model.Collector
			err := a.Stream(t.Context(), modelRequest("broken"), c.Add)
			if err == nil || strings.Contains(err.Error(), "private-diagnostic") {
				t.Fatal(err)
			}
			if mode == "partial-call" {
				if _, err := c.Response(); err == nil {
					t.Fatal("partial call became executable")
				}
			}
		})
	}
}

func TestPluginModelCancellationIsCorrelatedAndConfirmed(t *testing.T) {
	a, dir := modelFixture(t, "confirm-cancel")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- a.Stream(ctx, modelRequest("hold"), func(model.Delta) error { close(started); return nil })
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not started")
	}
	// Another stream can finish on the same instance while the first is active.
	var c model.Collector
	if err := a.Stream(t.Context(), modelRequest("other"), c.Add); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "cancel-received")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel not delivered")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatal("reported cancellation before confirmation", err)
	default:
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing cancellation result")
	}
	if a.instance.Info().State != "ready" {
		t.Fatal("cancellation closed unrelated plugin capabilities")
	}
}

func TestPluginModelFailureBoundsAndCompletionBeforeExit(t *testing.T) {
	t.Run("cancelled-observer", func(t *testing.T) {
		a, _ := modelFixture(t, "ok")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := a.Stream(ctx, modelRequest("hold-observer"), func(model.Delta) error { cancel(); return ctx.Err() })
		if !errors.Is(err, context.Canceled) || a.instance.Info().State != "ready" {
			t.Fatal("observer cancellation bypassed handshake", err, a.instance.Info())
		}
	})
	t.Run("ignored-cancellation", func(t *testing.T) {
		a, _ := modelFixture(t, "ignore-cancel")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := a.Stream(ctx, modelRequest("hold"), func(model.Delta) error { cancel(); return nil })
		requireFailure(t, err, "transport_unknown")
		if a.instance.Info().State != "failed" {
			t.Fatal(a.instance.Info())
		}
	})
	t.Run("observer", func(t *testing.T) {
		a, _ := modelFixture(t, "ok")
		wanted := errors.New("storage unavailable")
		if err := a.Stream(t.Context(), modelRequest("observer"), func(model.Delta) error { return wanted }); !errors.Is(err, wanted) {
			t.Fatal(err)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		a, _ := modelFixture(t, "overflow")
		var once sync.Once
		err := a.Stream(t.Context(), modelRequest("overflow"), func(model.Delta) error {
			once.Do(func() {
				select {
				case <-a.instance.Done():
				case <-time.After(5 * time.Second):
					t.Error("queue did not overflow")
				}
			})
			return nil
		})
		if err == nil || a.instance.Info().State != "failed" {
			t.Fatal(err, a.instance.Info())
		}
	})
	t.Run("complete-exit", func(t *testing.T) {
		a, _ := modelFixture(t, "complete-exit")
		var once sync.Once
		var c model.Collector
		err := a.Stream(t.Context(), modelRequest("exit"), func(d model.Delta) error {
			once.Do(func() {
				select {
				case <-a.instance.Done():
				case <-time.After(3 * time.Second):
					t.Error("fixture did not exit")
				}
			})
			return c.Add(d)
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestPluginModelQueueByteBudgetAndChannelIsolation(t *testing.T) {
	raw, _ := json.Marshal(ModelDelta{RequestID: "rpc", OperationID: "model", Sequence: 1, Delta: model.Delta{Kind: model.ResponseEnd}})
	frame := response{JSONRPC: "2.0", Method: "plugin.model.delta", Params: raw}
	for _, remaining := range []int{len(raw) - 1, len(raw)} {
		p := &pendingResponse{id: "rpc", operation: "model", notification: "plugin.model.delta", queuedBytes: MaxQueuedModelBytes - remaining, ch: make(chan response, MaxQueuedProgress+1)}
		i := &Instance{pending: map[string]*pendingResponse{"rpc": p}}
		ok := i.route(frame)
		if ok != (remaining == len(raw)) {
			t.Fatal("byte budget off by one", remaining, ok)
		}
	}
	p := &pendingResponse{id: "rpc", operation: "model", notification: "plugin.progress", ch: make(chan response, MaxQueuedProgress+1)}
	i := &Instance{pending: map[string]*pendingResponse{"rpc": p}}
	if i.route(frame) {
		t.Fatal("model delta entered a tool channel")
	}
}

func TestPluginModelPrivateDataIsScopedToInstallation(t *testing.T) {
	a, dir := modelFixture(t, "opaque")
	var c model.Collector
	if err := a.Stream(t.Context(), modelRequest("first"), c.Add); err != nil {
		t.Fatal(err)
	}
	response, err := c.Response()
	if err != nil {
		t.Fatal(err)
	}
	r := modelRequest("next")
	r.Messages = append(r.Messages, response.Messages...)
	// Same manifest and model in a different directory can load another
	// provider configuration; its private continuation must not cross over.
	other, err := ModelMetadata(filepath.Join(dir, "other", "plugin.json"), a.definition, a.info.ID, a.effort)
	if err != nil {
		t.Fatal(err)
	}
	if other.Protocol() == a.Protocol() {
		t.Fatal("different installations share opaque namespace")
	}
	requireFailure(t, other.ValidateContext(r), "protocol_mismatch")
	same, err := ModelMetadata(a.instance.source, a.definition, a.info.ID, a.effort)
	if err != nil || same.Protocol() != a.Protocol() || same.ValidateContext(r) != nil {
		t.Fatal("offline metadata changed original protocol", err)
	}
	requireFailure(t, same.Stream(t.Context(), r, func(model.Delta) error { return nil }), "unavailable")
}
