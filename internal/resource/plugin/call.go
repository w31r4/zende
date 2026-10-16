package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"zende.local/harness/internal/protocol"
)

type Scope struct {
	AgentID string `json:"agent_id,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	Cwd     string `json:"cwd"`
}

type CallBinding struct {
	PluginID        string `json:"plugin_id"`
	PluginVersion   string `json:"plugin_version"`
	ManifestVersion string `json:"manifest_version"`
	InstanceID      string `json:"instance_id"`
	CatalogVersion  string `json:"catalog_version"`
	DefinitionRef   string `json:"definition_ref"`
	OperationID     string `json:"operation_id"`
}

type CallFailure struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Completion confirms a terminal invocation, not rollback of external effects.
// Output belongs to the capability: tool content and command machine output
// are interpreted by their own adapters, not by the transport.
type Completion struct {
	OperationID string          `json:"operation_id"`
	State       string          `json:"state"`
	Output      json.RawMessage `json:"output,omitempty"`
	Error       *CallFailure    `json:"error,omitempty"`
}

type CallResult struct {
	CallBinding
	RequestID  string      `json:"request_id"`
	State      string      `json:"state"`
	Completion *Completion `json:"completion,omitempty"`
	// Evidence is the original bounded response, even if validation failed.
	// It is not suitable for a model context or ordinary diagnostics verbatim.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

type Call struct {
	instance  *Instance
	entry     callableEntry
	binding   CallBinding
	scope     Scope
	arguments json.RawMessage
	used      atomic.Bool
}

func (c *Call) Binding() CallBinding       { return c.binding }
func (c *Call) Scope() Scope               { return c.scope }
func (c *Call) Definition() Definition     { return cloneDefinition(c.entry.definition) }
func (c *Call) Arguments() json.RawMessage { return bytes.Clone(c.arguments) }

// PrepareCall is local and side-effect free. The owner must journal this
// binding, scope, definition and dispatch intent durably BEFORE Invoke, and
// retain the instance until Invoke returns. It must never replay an unknown call.
func (i *Instance) PrepareCall(ref, operation string, scope Scope, arguments json.RawMessage) (*Call, error) {
	if operation == "" || len(operation) > 128 || !utf8.ValidString(operation) || strings.IndexFunc(operation, func(r rune) bool { return r < 33 || r == 127 }) >= 0 {
		return nil, protocol.E("invalid_params", "plugin call needs a bounded operation ID")
	}
	if scope.Cwd == "" {
		scope.Cwd = i.cmd.Dir
	}
	if !filepath.IsAbs(scope.Cwd) || filepath.Clean(scope.Cwd) != scope.Cwd || len(scope.Cwd) > 4096 || !utf8.ValidString(scope.Cwd) || strings.ContainsRune(scope.Cwd, 0) {
		return nil, protocol.E("invalid_params", "plugin call cwd must be an absolute clean path")
	}
	for _, id := range []string{scope.AgentID, scope.TaskID} {
		if len(id) > 128 || !utf8.ValidString(id) || strings.IndexFunc(id, func(r rune) bool { return r < 33 || r == 127 }) >= 0 {
			return nil, protocol.E("invalid_params", "invalid plugin call scope")
		}
	}
	i.mu.Lock()
	info, catalog := i.info, i.catalog
	i.mu.Unlock()
	if info.State != "ready" || catalog == nil {
		return nil, protocol.E("unavailable", "discover plugin capabilities before preparing a call")
	}
	var entry *callableEntry
	for n := range catalog.entries {
		if catalog.entries[n].definition.Ref == ref {
			entry = &catalog.entries[n]
			break
		}
	}
	if entry == nil {
		return nil, protocol.E("stale_reference", "plugin definition is unavailable on this instance; discover the current version")
	}
	v, err := decodeValue(arguments, MaxArgumentBytes)
	if err != nil {
		return nil, err
	}
	if entry.input.Validate(v) != nil {
		return nil, protocol.E("invalid_arguments", "plugin arguments do not match the selected schema")
	}
	return &Call{instance: i, entry: *entry, binding: CallBinding{info.ID, info.Version, info.ManifestVersion, info.InstanceID, catalog.version, ref, operation}, scope: scope, arguments: bytes.Clone(arguments)}, nil
}

// Invoke is one-shot. emit runs in wire order, outside transport locks, and
// should durably retain progress before returning. A failed observer makes the
// outcome unknown. Cancelling ctx requests cancellation and waits a bounded
// time for the plugin's terminal response; it does not fabricate a cancelled result.
func (c *Call) Invoke(ctx context.Context, emit func(Progress) error) (CallResult, error) {
	result := CallResult{CallBinding: c.binding, State: "not_dispatched"}
	if !c.used.CompareAndSwap(false, true) {
		result.State = "unknown"
		return result, protocol.E("operation_conflict", "plugin invocation was already attempted; inspect its original result")
	}
	if emit == nil {
		return result, protocol.E("invalid_params", "plugin invocation needs a progress observer")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	params := struct {
		OperationID   string          `json:"operation_id"`
		DefinitionRef string          `json:"definition_ref"`
		Name          string          `json:"name"`
		Scope         Scope           `json:"scope"`
		Arguments     json.RawMessage `json:"arguments"`
	}{c.binding.OperationID, c.binding.DefinitionRef, c.entry.definition.Name, c.scope, c.arguments}
	p, sent, err := c.instance.startRequest(ctx, "plugin."+c.entry.definition.Kind+".call", params, c.binding.OperationID)
	result.RequestID = p.id
	if sent {
		result.State = "unknown"
	}
	unknown := func(message string) (CallResult, error) {
		result.State = "unknown"
		return result, protocol.E("operation_unknown", message)
	}
	if err != nil {
		if sent {
			return unknown("plugin request may have been dispatched; inspect the original operation")
		}
		return result, err
	}
	defer c.instance.forget(p)
	cancelled := ctx.Done()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var reply response
		c.instance.mu.Lock()
		complete := p.complete
		c.instance.mu.Unlock()
		if !complete {
			select {
			case <-deadline:
				c.instance.stop(protocol.E("timeout", "plugin did not confirm cancellation before the deadline"))
				return unknown("plugin cancellation is unconfirmed; inspect the original operation before retrying")
			default:
			}
		}
		// Check cancellation even while progress is continuous.
		if cancelled != nil {
			select {
			case <-cancelled:
				cancelled = nil
				if !complete {
					if err := c.instance.cancelRequest(p); err != nil {
						c.instance.stop(protocol.E("unavailable", "plugin cancellation could not be delivered"))
					}
				}
				timer = time.NewTimer(CancelTimeout)
				deadline = timer.C
			default:
			}
		}
		select {
		case reply = <-p.ch:
		default:
			select {
			case reply = <-p.ch:
			case <-cancelled:
				continue
			case <-deadline:
				c.instance.stop(protocol.E("timeout", "plugin did not confirm cancellation before the deadline"))
				return unknown("plugin cancellation is unconfirmed; inspect the original operation before retrying")
			case <-c.instance.done:
				// A complete reply read before EOF is still authoritative.
				select {
				case reply = <-p.ch:
				default:
					return unknown("plugin exited before a verifiable result; inspect the original operation")
				}
			}
		}
		if reply.Method != "" {
			var progress Progress
			_ = json.Unmarshal(reply.Params, &progress) // validated and correlated by the reader
			if err := emit(progress); err != nil {
				c.instance.stop(protocol.E("unavailable", "plugin progress could not be retained"))
				return unknown("plugin progress retention failed; inspect the original operation")
			}
			continue
		}
		if len(reply.Error) > 0 {
			result.Evidence = bytes.Clone(reply.Error)
			return unknown("plugin returned a protocol error without an invocation outcome; inspect diagnostics and original operation")
		}
		result.Evidence = bytes.Clone(reply.Result)
		completion, err := c.validateCompletion(reply.Result)
		if err != nil {
			return unknown("plugin returned an invalid completion; retained response is evidence only")
		}
		result.State = completion.State
		result.Completion = &completion
		return result, nil
	}
}

func (c *Call) validateCompletion(raw []byte) (Completion, error) {
	return validateCompletion(c.entry, c.binding.OperationID, raw)
}

// ValidateCompletion checks retained evidence with the original definition,
// without a live process. A successful check does not authorize re-execution.
func (c *Catalog) ValidateCompletion(ref, operation string, raw []byte) (Completion, error) {
	for _, entry := range c.entries {
		if entry.definition.Ref == ref {
			return validateCompletion(entry, operation, raw)
		}
	}
	return Completion{}, protocol.E("stale_reference", "plugin definition is absent from the saved catalog")
}

func (c *Catalog) ValidateArguments(ref string, raw []byte) error {
	value, err := decodeValue(raw, MaxArgumentBytes)
	if err != nil {
		return err
	}
	for _, entry := range c.entries {
		if entry.definition.Ref == ref {
			if entry.input.Validate(value) != nil {
				return protocol.E("invalid_arguments", "plugin arguments do not match the selected schema")
			}
			return nil
		}
	}
	return protocol.E("stale_reference", "plugin definition is absent from the saved catalog")
}

func validateCompletion(entry callableEntry, operation string, raw []byte) (Completion, error) {
	var out Completion
	invalid := protocol.E("plugin_protocol", "invalid plugin completion")
	if _, err := decodeValue(raw, MaxResultBytes); err != nil {
		return out, invalid
	}
	if protocol.StrictDecode(raw, &out) != nil || out.OperationID != operation {
		return out, invalid
	}
	switch out.State {
	case "completed":
		if out.Error != nil || len(out.Output) == 0 {
			return out, invalid
		}
	case "failed":
		if out.Error == nil || !callableName.MatchString(out.Error.Kind) || out.Error.Message == "" || len(out.Error.Message) > 8192 {
			return out, invalid
		}
	case "cancelled":
		if out.Error != nil {
			return out, invalid
		}
	default:
		return out, invalid
	}
	if out.State == "completed" && entry.output != nil {
		v, err := decodeValue(out.Output, MaxResultBytes)
		if err != nil || entry.output.Validate(v) != nil {
			return out, invalid
		}
	}
	return out, nil
}
