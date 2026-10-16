package plugin

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Direction names the source. Core capabilities consume original Core events;
// plugin capabilities produce schema-checked data, never Core state changes.
type EventCapability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Direction   string          `json:"direction"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

type EventDefinition struct {
	EventCapability
	Ref string `json:"ref"`
}

type eventEntry struct {
	definition EventDefinition
	schema     *jsonschema.Schema
}

func (c *Catalog) Events() []EventDefinition {
	var out []EventDefinition
	for _, entry := range c.events {
		d := entry.definition
		d.Schema = bytes.Clone(d.Schema)
		out = append(out, d)
	}
	return out
}

func (c *Catalog) compileEvents(info Info, definitions []EventCapability) error {
	if len(definitions) > 0 && !slices.Contains(info.Capabilities, "events") {
		return invalidCatalog()
	}
	seen := map[string]bool{}
	for _, d := range definitions {
		key := d.Direction + ":" + d.Name
		if !callableName.MatchString(d.Name) || seen[key] || strings.TrimSpace(d.Description) == "" || len(d.Description) > 8192 {
			return invalidCatalog()
		}
		seen[key] = true
		var schema *jsonschema.Schema
		switch d.Direction {
		case "core":
			if len(d.Schema) != 0 {
				return invalidCatalog() // Core retains its own event schema.
			}
		case "plugin":
			var err error
			schema, err = compileSchema(d.Schema)
			if err != nil {
				return invalidCatalog()
			}
		default:
			return invalidCatalog()
		}
		raw, _ := json.Marshal(d)
		value, err := decodeValue(raw, MaxDefinitionBytes)
		if err != nil {
			return invalidCatalog()
		}
		canonical, _ := json.Marshal(value)
		if json.Unmarshal(canonical, &d) != nil {
			return invalidCatalog()
		}
		ref := "plugin_event_" + digest([]any{info.ID, info.ManifestVersion, "event", json.RawMessage(canonical)})
		c.events = append(c.events, eventEntry{EventDefinition{EventCapability: d, Ref: ref}, schema})
	}
	slices.SortFunc(c.events, func(a, b eventEntry) int {
		return strings.Compare(a.definition.Direction+":"+a.definition.Name, b.definition.Direction+":"+b.definition.Name)
	})
	return nil
}
