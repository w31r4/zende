package mcp

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"zende.local/harness/internal/protocol"
)

type parameterHeaderKey struct{}

// ApplyCallHeaders belongs on the managed HTTP transport's cloned request.
// SDK CallTool normally uses its latest name-based cache for these headers;
// our sending middleware removes that context and supplies the selected
// definition instead. Configuration headers must not override Mcp-Param-*.
func ApplyCallHeaders(req *http.Request) {
	if req.Header.Get("Mcp-Protocol-Version") < "2026-07-28" || req.Header.Get("Mcp-Method") != "tools/call" {
		return
	}
	if headers, ok := req.Context().Value(parameterHeaderKey{}).(map[string]string); ok {
		for key := range req.Header {
			if strings.HasPrefix(strings.ToLower(key), "mcp-param-") {
				req.Header.Del(key)
			}
		}
		for name, value := range headers {
			req.Header.Set(name, value)
		}
	}
}

// The supported MCP header annotation addresses nested object properties.
// Values follow SEP-2243: strings, booleans and JS-safe integers; leading or
// trailing whitespace, non-ASCII/control bytes and base64 sentinels are encoded.
// Definitions have already passed the SDK's annotation validation on listing.
func parameterHeaders(schema any, arguments map[string]any) (map[string]string, error) {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, invalidCatalog()
	}
	var root map[string]any
	if json.Unmarshal(b, &root) != nil {
		return nil, invalidCatalog()
	}
	headers := map[string]string{}
	total := 0
	var walk func(map[string]any, map[string]any) error
	walk = func(node, args map[string]any) error {
		props, _ := node["properties"].(map[string]any)
		for key, raw := range props {
			property, _ := raw.(map[string]any)
			value, exists := args[key]
			if !exists || value == nil {
				continue
			}
			if name, ok := property["x-mcp-header"].(string); ok && name != "" {
				encoded, ok := headerValue(value)
				if !ok {
					return protocol.E("invalid_arguments", "MCP header argument must be a string, boolean or safe integer")
				}
				name = http.CanonicalHeaderKey("Mcp-Param-" + name)
				if _, exists := headers[name]; exists {
					return invalidCatalog()
				}
				total += len(name) + len(encoded)
				if total > 16<<10 {
					return protocol.E("invalid_arguments", "MCP parameter headers exceed 16 KiB")
				}
				headers[name] = encoded
			}
			if object, ok := value.(map[string]any); ok {
				if err := walk(property, object); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, arguments); err != nil {
		return nil, err
	}
	return headers, nil
}

func headerValue(value any) (string, bool) {
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case bool:
		text = strconv.FormatBool(v)
	case json.Number:
		if !boundedNumber(v) {
			return "", false
		}
		n, ok := new(big.Rat).SetString(v.String())
		if !ok || !n.IsInt() || !n.Num().IsInt64() {
			return "", false
		}
		i := n.Num().Int64()
		if i < -(1<<53-1) || i > 1<<53-1 {
			return "", false
		}
		text = strconv.FormatInt(i, 10)
	default:
		return "", false
	}
	encode := strings.Trim(text, " \t") != text || strings.HasPrefix(text, "=?base64?") && strings.HasSuffix(text, "?=")
	for _, r := range text {
		if r < 32 || r > 126 {
			encode = true
			break
		}
	}
	if encode {
		text = "=?base64?" + base64.StdEncoding.EncodeToString([]byte(text)) + "?="
	}
	return text, true
}
