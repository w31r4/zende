// Package anthropic adapts the Messages API to the shared model stream.
package anthropic

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const Protocol = "anthropic"

type Config struct {
	Endpoint, Effort, Thinking, ThinkingDisplay string
	ThinkingBudget                              int
	InterleavedThinking, Images                 bool
	// ClientToolSearch requires an explicitly confirmed endpoint/model. The
	// custom host search returns tool_reference blocks, not server-side search.
	ClientToolSearch bool
	Credential       func(context.Context) (string, error)
	Client           *http.Client
}

type Adapter struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config) (*Adapter, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, protocol.E("invalid_config", "Anthropic endpoint must be an HTTP API base URL without credentials, query or fragment")
	}
	switch config.Thinking {
	case "", "disabled", "adaptive":
		if config.ThinkingBudget != 0 || config.InterleavedThinking {
			return nil, protocol.E("invalid_config", "thinking budget and interleaving beta require enabled thinking")
		}
	case "enabled":
		if config.ThinkingBudget < 1024 {
			return nil, protocol.E("invalid_config", "enabled thinking requires a budget of at least 1024 tokens")
		}
	default:
		return nil, protocol.E("invalid_config", "unsupported Anthropic thinking mode")
	}
	if config.ThinkingDisplay != "" && (config.ThinkingDisplay != "summarized" && config.ThinkingDisplay != "omitted" || config.Thinking != "enabled" && config.Thinking != "adaptive") {
		return nil, protocol.E("invalid_config", "thinking display requires enabled or adaptive thinking")
	}
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
	var client http.Client
	if config.Client != nil {
		client = *config.Client
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		client.Transport = transport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Adapter{config: config, endpoint: u.String(), client: &client}, nil
}

func (a *Adapter) ValidateRequest(request model.Request) error {
	_, err := a.encode(request, false)
	return err
}

func (a *Adapter) ValidateContext(request model.Request) error {
	_, err := a.encode(request, true)
	return err
}

func (a *Adapter) Stream(ctx context.Context, request model.Request, emit func(model.Delta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := a.encode(request, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/messages", bytes.NewReader(body))
	if err != nil {
		return protocol.E("invalid_config", "Anthropic request could not be constructed")
	}
	if a.config.Credential != nil {
		key, err := a.config.Credential(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || strings.ContainsAny(key, "\r\n") {
			return protocol.E("credential_unavailable", "Anthropic credential could not be resolved")
		}
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	if a.config.InterleavedThinking {
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	response, err := a.client.Do(req)
	if ctx.Err() != nil {
		if response != nil {
			response.Body.Close()
		}
		return ctx.Err()
	}
	if err != nil {
		return protocol.E("model_transport", "Anthropic connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return model.HTTPFailure("Anthropic", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return wireError("expected text/event-stream")
	}
	d := decoder{endpoint: a.endpoint, model: request.Model, emit: emit, clientSearch: a.config.ClientToolSearch}
	for _, tool := range request.Tools {
		if tool.Name == searchToolName {
			d.searchAdvertised = true
		}
	}
	err = readSSE(response.Body, d.receive)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func wireError(message string) error { return protocol.E("model_protocol", "Anthropic: "+message) }
