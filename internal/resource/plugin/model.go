package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

// Model requests include resolved images, which can exceed the inbound frame
// budget after base64 encoding. Only this outbound method has the larger limit.
const MaxModelRequestBytes = 32 << 20
const MaxQueuedModelBytes = 8 << 20

type ModelDelta struct {
	RequestID   string      `json:"request_id"`
	OperationID string      `json:"operation_id"` // model.Request.ID, not the RPC ID
	Sequence    int64       `json:"sequence"`
	Delta       model.Delta `json:"delta"`
}

type ModelCompletion struct {
	OperationID string       `json:"operation_id"`
	State       string       `json:"state"`
	Error       *CallFailure `json:"error,omitempty"`
}

type ModelImage struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

type ModelParams struct {
	OperationID   string                `json:"operation_id"`
	DefinitionRef string                `json:"definition_ref"`
	Name          string                `json:"name"`
	Effort        string                `json:"effort,omitempty"`
	Protocol      string                `json:"protocol"`
	Scope         Scope                 `json:"scope"`
	Request       model.Request         `json:"request"`
	Images        map[string]ModelImage `json:"images,omitempty"`
}

// ModelAdapter is an immutable selection on an already discovered instance.
// The owner retains that instance until Stream returns. Model generation has
// its own semantic channel; it is never wrapped in a tool Completion.Output.
type ModelAdapter struct {
	instance   *Instance
	definition ProviderDefinition
	info       protocol.ModelInfo
	binding    CallBinding
	scope      Scope
	effort     string
	protocol   string
}

var _ model.Adapter = (*ModelAdapter)(nil)
var _ model.RequestValidator = (*ModelAdapter)(nil)
var _ model.ContextValidator = (*ModelAdapter)(nil)

func (i *Instance) Model(ref, name, effort string, scope Scope) (*ModelAdapter, error) {
	scope, err := i.validateScope(scope)
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	info, catalog := i.info, i.catalog
	i.mu.Unlock()
	if info.State != "ready" || catalog == nil {
		return nil, protocol.E("unavailable", "discover plugin providers before selecting a model")
	}
	for _, d := range catalog.Providers() {
		if d.Ref != ref {
			continue
		}
		a, err := ModelMetadata(i.source, d, name, effort)
		if err != nil {
			return nil, err
		}
		a.instance, a.scope = i, scope
		a.binding = CallBinding{PluginID: info.ID, PluginVersion: info.Version, ManifestVersion: info.ManifestVersion, InstanceID: info.InstanceID, CatalogVersion: catalog.version, DefinitionRef: ref}
		return a, nil
	}
	return nil, protocol.E("stale_reference", "plugin provider or model changed; discover the current catalog")
}

// ModelMetadata reconstructs preflight settings from a verified catalog entry.
// It owns no process and cannot stream. Retained history can therefore be
// checked even after its old instance retires; dispatch separately requires a
// live exact-version binding. Source identity isolates different installations.
func ModelMetadata(source string, d ProviderDefinition, name, effort string) (*ModelAdapter, error) {
	if !filepath.IsAbs(source) || filepath.Clean(source) != source || len(source) > 4096 || !utf8.ValidString(source) || strings.ContainsRune(source, 0) {
		return nil, protocol.E("invalid_params", "plugin model source must be an absolute clean path")
	}
	for _, m := range d.Models {
		if m.ID != name {
			continue
		}
		if effort != "" && !slices.Contains(m.Efforts, effort) {
			return nil, protocol.E("unsupported_capability", "plugin model does not support the selected effort")
		}
		m.Efforts = slices.Clone(m.Efforts)
		return &ModelAdapter{definition: d, info: m, effort: effort,
			protocol: "plugin-model/" + d.Ref + "/" + digest([]string{source, name})}, nil
	}
	return nil, protocol.E("model_unavailable", "plugin model is absent from its saved definition")
}

func (a *ModelAdapter) Binding() CallBinding { return a.binding }
func (a *ModelAdapter) Protocol() string     { return a.protocol }
func (a *ModelAdapter) Info() protocol.ModelInfo {
	m := a.info
	m.Efforts = slices.Clone(m.Efforts)
	return m
}

func (a *ModelAdapter) params(r model.Request) ModelParams {
	p := ModelParams{OperationID: r.ID, DefinitionRef: a.definition.Ref, Name: a.definition.Name, Effort: a.effort, Protocol: a.protocol, Scope: a.scope, Request: r}
	if len(r.Images) > 0 {
		p.Images = make(map[string]ModelImage, len(r.Images))
		for id, image := range r.Images {
			p.Images[id] = ModelImage{MediaType: image.MediaType, Data: image.Data}
		}
	}
	return p
}

func (a *ModelAdapter) Stream(ctx context.Context, r model.Request, emit func(model.Delta) error) error {
	if a.instance == nil {
		return protocol.E("unavailable", "plugin model metadata has no live instance")
	}
	if emit == nil {
		return protocol.E("invalid_params", "plugin model stream needs an observer")
	}
	if err := a.ValidateRequest(r); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p, sent, err := a.instance.startRequest(ctx, "plugin.model.stream", a.params(r), r.ID)
	if err != nil {
		if sent {
			return protocol.E("transport_unknown", "plugin model request may have been sent; it was not retried")
		}
		return err
	}
	defer a.instance.forget(p)
	var collector model.Collector
	cancelled := ctx.Done()
	var deadline <-chan time.Time
	var timer *time.Timer
	observerCancelled := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	unknown := func() error {
		return protocol.E("transport_unknown", "plugin model stream ended without a verified terminal result; it was not retried")
	}
	fail := func(message string) error {
		err := protocol.E("plugin_protocol", message)
		a.instance.stop(err)
		return err
	}
	for {
		a.instance.mu.Lock()
		complete := p.complete
		a.instance.mu.Unlock()
		if !complete {
			select {
			case <-deadline:
				a.instance.stop(protocol.E("timeout", "plugin model cancellation was not confirmed"))
				return unknown()
			default:
			}
		}
		if cancelled != nil {
			select {
			case <-cancelled:
				cancelled = nil
				if !complete {
					if a.instance.cancelRequest(p) != nil {
						a.instance.stop(protocol.E("unavailable", "plugin model cancellation could not be delivered"))
					}
				}
				timer = time.NewTimer(CancelTimeout)
				deadline = timer.C
			default:
			}
		}
		var reply response
		select {
		case reply = <-p.ch:
		default:
			select {
			case reply = <-p.ch:
			case <-cancelled:
				continue
			case <-deadline:
				a.instance.stop(protocol.E("timeout", "plugin model cancellation was not confirmed"))
				return unknown()
			case <-a.instance.done:
				select {
				case reply = <-p.ch:
				default:
					return unknown()
				}
			}
		}
		if reply.Method != "" {
			a.instance.mu.Lock()
			p.queuedBytes -= len(reply.Params)
			a.instance.mu.Unlock()
			var event ModelDelta
			_ = json.Unmarshal(reply.Params, &event) // reader validated envelope and correlation
			d := event.Delta
			// The runtime observer journals even semantically invalid output. No
			// tool may run until both this stream and its collector finish.
			if !observerCancelled {
				if err := emit(d); err != nil {
					if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
						// Runtime observers also see cancellation. Drain the
						// bounded handshake without killing unrelated requests.
						observerCancelled = true
					} else {
						a.instance.stop(protocol.E("unavailable", "plugin model output observer failed"))
						return err
					}
				}
			}
			if d.Kind == model.ProtocolData && d.Protocol != a.protocol {
				return fail("plugin model emitted foreign protocol data")
			}
			if err := collector.Add(d); err != nil {
				return fail("plugin model emitted invalid public model events")
			}
			continue
		}
		if len(reply.Error) > 0 {
			return protocol.E("model_error", "plugin rejected the model request; inspect its diagnostics")
		}
		var completion ModelCompletion
		if len(reply.Result) > MaxProgressBytes || protocol.StrictDecode(reply.Result, &completion) != nil || completion.OperationID != r.ID {
			return fail("plugin model returned an invalid terminal result")
		}
		switch completion.State {
		case "completed":
			if completion.Error != nil {
				return fail("completed plugin model request contains an error")
			}
			if _, err := collector.Response(); err != nil {
				return fail("plugin model completed without a complete public response")
			}
			if observerCancelled {
				return ctx.Err()
			}
			return nil
		case "cancelled":
			if completion.Error != nil {
				return fail("cancelled plugin model request contains an error")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return protocol.E("cancelled", "plugin cancelled the model request")
		case "failed":
			if completion.Error == nil || !callableName.MatchString(completion.Error.Kind) || completion.Error.Message == "" || len(completion.Error.Message) > 8192 {
				return fail("failed plugin model request lacks a bounded error")
			}
			// Plugin diagnostic text can contain credentials; do not reflect it
			// into public errors. The plugin can emit redacted stderr diagnostics.
			return protocol.E("model_error", "plugin model request failed; inspect its diagnostics")
		default:
			return fail("plugin model terminal state is invalid")
		}
	}
}
