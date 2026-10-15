package tool

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Registry is an immutable request-scoped set. Resource publication can replace
// the registry later; an in-flight request retains its original executors.
type Registry struct{ entries map[string]registered }
type registered struct {
	executor   Executor
	descriptor Descriptor
}

func NewRegistry(executors ...Executor) (*Registry, error) {
	r := &Registry{entries: map[string]registered{}}
	for _, executor := range executors {
		if executor == nil {
			return nil, fmt.Errorf("nil tool")
		}
		d := executor.Descriptor()
		var schema map[string]any
		if d.Name == "" || d.Version == "" || json.Unmarshal(d.InputSchema, &schema) != nil || schema["type"] != "object" {
			return nil, fmt.Errorf("tool needs a name, version and object input schema")
		}
		if _, exists := r.entries[d.Name]; exists {
			return nil, fmt.Errorf("duplicate tool name")
		}
		r.entries[d.Name] = registered{executor: executor, descriptor: cloneDescriptor(d)}
	}
	return r, nil
}

func cloneDescriptor(d Descriptor) Descriptor {
	d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	d.Guidelines = append([]string(nil), d.Guidelines...)
	return d
}

func (r *Registry) Descriptors() []Descriptor {
	result := []Descriptor{}
	if r == nil {
		return result
	}
	for _, entry := range r.entries {
		result = append(result, cloneDescriptor(entry.descriptor))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (r *Registry) Lookup(name string) (Executor, Descriptor, bool) {
	if r == nil {
		return nil, Descriptor{}, false
	}
	e, ok := r.entries[name]
	return e.executor, cloneDescriptor(e.descriptor), ok
}
