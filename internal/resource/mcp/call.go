package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"zende.local/harness/internal/protocol"
)

const (
	MaxArgumentBytes = 1 << 20
	MaxResultBytes   = 16 << 20
	MaxContentBlocks = 1024
)

// Call is a single-use, validated invocation of one immutable definition.
// Preparing it has no remote effect. The host must retain the connection until
// Invoke returns and journal dispatch BEFORE invoking it. MCP does not provide
// an exactly-once key: a lost reply must never be retried automatically.
type Call struct {
	connection *Connection
	definition Definition
	catalog    string
	name       string
	arguments  json.RawMessage
	output     *jsonschema.Schema
	headers    map[string]string
	used       atomic.Bool
}

// CallResult retains the complete supported MCP result, including content,
// structuredContent, annotations and metadata. Binary image/audio/resource
// data remain in their MCP representation; the host owns durable attachments
// and context budgeting. A nonempty Result accompanying an error is evidence,
// not a validated success, and must not be silently discarded or replayed.
type CallResult struct {
	ToolRef        string          `json:"tool_ref"`
	CatalogVersion string          `json:"catalog_version"`
	Result         json.RawMessage `json:"result,omitempty"`
}

func (c *Call) Definition() Definition     { return copyDefinition(c.definition) }
func (c *Call) Arguments() json.RawMessage { return bytes.Clone(c.arguments) }
func (c *Call) CatalogVersion() string     { return c.catalog }

func (c *Connection) PrepareCall(ctx context.Context, ref string, arguments json.RawMessage) (*Call, error) {
	if ctx.Err() != nil {
		return nil, protocol.E("cancelled", "MCP call was not dispatched")
	}
	snapshot, err := c.Snapshot()
	if err != nil {
		return nil, err
	}
	definition, err := snapshot.Lookup(ref)
	if err != nil {
		return nil, err
	}
	value, err := decodeCallObject(arguments)
	if err != nil {
		return nil, err
	}
	var tool mcpsdk.Tool
	if json.Unmarshal(definition.Tool, &tool) != nil {
		return nil, invalidCatalog()
	}
	input, err := compileSchema(tool.InputSchema)
	if err != nil {
		return nil, invalidCatalog()
	}
	if input.Validate(value) != nil {
		// Schema diagnostics may contain argument/credential values.
		return nil, protocol.E("invalid_arguments", "MCP arguments do not match the selected input schema; inspect the definition")
	}
	var output *jsonschema.Schema
	if tool.OutputSchema != nil {
		output, err = compileSchema(tool.OutputSchema)
		if err != nil {
			return nil, invalidCatalog()
		}
	}
	headers, err := parameterHeaders(tool.InputSchema, value)
	if err != nil {
		return nil, err
	}
	return &Call{connection: c, definition: definition, catalog: snapshot.Version(), name: tool.Name,
		arguments: bytes.Clone(arguments), output: output, headers: headers}, nil
}

func decodeCallObject(raw []byte) (map[string]any, error) {
	invalid := func() (map[string]any, error) {
		return nil, protocol.E("invalid_arguments", "MCP arguments require a UTF-8 object without duplicate keys; limits: 1 MiB, depth 64, numbers 1024 characters and exponent +/-1024")
	}
	if len(raw) == 0 || len(raw) > MaxArgumentBytes || !utf8.Valid(raw) {
		return invalid()
	}
	// Token decoding rejects duplicate keys rather than validating one value
	// while a server interprets another. UseNumber preserves large integers.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := decodeCallValue(d, 0)
	if err != nil {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return invalid()
	}
	return obj, nil
}

func decodeCallValue(d *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, io.ErrUnexpectedEOF
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		v := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			if _, exists := v[name]; exists {
				return nil, io.ErrUnexpectedEOF
			}
			value, err := decodeCallValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			v[name] = value
		}
		_, err := d.Token()
		return v, err
	case json.Delim('['):
		v := []any{}
		for d.More() {
			value, err := decodeCallValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			v = append(v, value)
		}
		_, err := d.Token()
		return v, err
	default:
		if n, ok := t.(json.Number); ok && !boundedNumber(n) {
			return nil, io.ErrUnexpectedEOF
		}
		if _, delimiter := t.(json.Delim); delimiter {
			return nil, io.ErrUnexpectedEOF
		}
		return t, nil
	}
}

func boundedNumber(n json.Number) bool {
	s := n.String()
	if len(s) > 1024 {
		return false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(s[i+1:])
		return err == nil && exponent >= -1024 && exponent <= 1024
	}
	return true
}

func (c *Call) Invoke(ctx context.Context) (CallResult, error) {
	result := CallResult{ToolRef: c.definition.Ref, CatalogVersion: c.catalog}
	if !c.used.CompareAndSwap(false, true) {
		return result, protocol.E("operation_conflict", "MCP call has already been attempted; inspect its original result")
	}
	if ctx.Err() != nil {
		return result, protocol.E("cancelled", "MCP call was not dispatched")
	}
	live, err := c.connection.Snapshot()
	if err != nil {
		return result, err
	}
	if _, err := live.Lookup(c.definition.Ref); err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.connection.life, cancel)
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil {
		return result, protocol.E("cancelled", "MCP call was not dispatched")
	}
	// CallTool injects a tool-name cache entry into its context. Replace that
	// context in middleware, keeping request metadata but using only headers
	// derived from OUR pinned definition, even after a partial catalog refresh.
	ctx = context.WithValue(ctx, parameterHeaderKey{}, c.headers)
	ctx = context.WithValue(ctx, callContextKey{}, ctx)
	reply, err := c.connection.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: c.name, Arguments: json.RawMessage(bytes.Clone(c.arguments))})
	if err != nil {
		return result, unknownCall("MCP call returned without a verifiable result; inspect the original operation before retrying")
	}
	if reply == nil {
		return result, unknownCall("MCP call returned no result; inspect the original operation")
	}
	raw, err := json.Marshal(reply)
	if err != nil || len(raw) > MaxResultBytes {
		return result, unknownCall("MCP result could not be retained within the 16 MiB limit; inspect the original operation")
	}
	result.Result = raw
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, decodeErr := decodeCallValue(d, 0)
	var shape struct {
		ResultType string `json:"resultType"`
	}
	_ = json.Unmarshal(raw, &shape)
	if decodeErr != nil || !boundedJSON(value, 0) || shape.ResultType != "" && shape.ResultType != "complete" || !validCallResult(reply, c.output) {
		return result, unknownCall("MCP returned an invalid or unsupported result; retained response is evidence only, not a retry instruction")
	}
	return result, nil
}

func unknownCall(message string) error { return protocol.E("operation_unknown", message) }

func validCallResult(r *mcpsdk.CallToolResult, output *jsonschema.Schema) bool {
	if len(r.Content) > MaxContentBlocks || len(r.InputRequests) != 0 || r.RequestState != "" || r.NeedsInput() {
		return false
	}
	for _, block := range r.Content {
		switch b := block.(type) {
		case *mcpsdk.TextContent:
			if b == nil || !utf8.ValidString(b.Text) {
				return false
			}
		case *mcpsdk.ImageContent:
			if b == nil || b.MIMEType == "" || len(b.Data) == 0 {
				return false
			}
		case *mcpsdk.AudioContent:
			if b == nil || b.MIMEType == "" || len(b.Data) == 0 {
				return false
			}
		case *mcpsdk.ResourceLink:
			if b == nil || b.URI == "" || b.Name == "" {
				return false
			}
		case *mcpsdk.EmbeddedResource:
			if b == nil || b.Resource == nil || b.Resource.URI == "" {
				return false
			}
		default:
			return false // sampling tool-use content is not a tool result
		}
	}
	if output != nil && !r.IsError {
		b, err := json.Marshal(r.StructuredContent)
		if err != nil {
			return false
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var value any
		if d.Decode(&value) != nil || !boundedJSON(value, 0) || output.Validate(value) != nil {
			return false
		}
	}
	return true
}

type callContextKey struct{}

func pinnedCallContext(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		if method == "tools/call" {
			if original, ok := ctx.Value(callContextKey{}).(context.Context); ok {
				ctx = original
			}
		}
		return next(ctx, method, req)
	}
}
