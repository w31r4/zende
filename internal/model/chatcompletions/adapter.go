// Package chatcompletions implements streaming Chat Completions without owning
// tool execution, task completion or provider credentials.
package chatcompletions

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const Protocol = "chat-completions"

// Compatibility is explicit per endpoint/model; no vendor-name heuristics or
// automatic resubmission after an HTTP rejection.
type Config struct {
	Endpoint, Effort, MaxTokensField, InstructionRole, ReasoningField string
	IncludeUsage, Images                                              bool
	Credential                                                        func(context.Context) (string, error)
	Client                                                            *http.Client
}

type Adapter struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config) (*Adapter, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, protocol.E("invalid_config", "Chat Completions endpoint must be an HTTP API base URL without credentials, query or fragment")
	}
	if config.MaxTokensField == "" {
		config.MaxTokensField = "max_completion_tokens"
	}
	if config.InstructionRole == "" {
		config.InstructionRole = "system"
	}
	if config.MaxTokensField != "max_completion_tokens" && config.MaxTokensField != "max_tokens" || config.InstructionRole != "system" && config.InstructionRole != "developer" || config.ReasoningField != "" && config.ReasoningField != "reasoning_content" && config.ReasoningField != "reasoning" && config.ReasoningField != "reasoning_text" {
		return nil, protocol.E("invalid_config", "invalid Chat Completions compatibility settings")
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return protocol.E("invalid_config", "Chat Completions request could not be constructed")
	}
	if a.config.Credential != nil {
		key, err := a.config.Credential(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || strings.ContainsAny(key, "\r\n") {
			return protocol.E("credential_unavailable", "Chat Completions credential could not be resolved")
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if request.ID != "" && len(request.ID) <= 128 && !strings.ContainsAny(request.ID, "\r\n") {
		req.Header.Set("X-Client-Request-Id", request.ID)
	}
	response, err := a.client.Do(req)
	if ctx.Err() != nil {
		if response != nil {
			response.Body.Close()
		}
		return ctx.Err()
	}
	if err != nil {
		return protocol.E("model_transport", "Chat Completions connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		kind := "model_http"
		switch response.StatusCode {
		case 401, 403:
			kind = "model_auth"
		case 429:
			kind = "model_rate_limit"
		case 408, 504:
			kind = "model_timeout"
		}
		return protocol.E(kind, fmt.Sprintf("Chat Completions HTTP %d", response.StatusCode))
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return wireError("expected text/event-stream")
	}
	d := decoder{endpoint: a.endpoint, model: request.Model, reasoningField: a.config.ReasoningField, emit: emit}
	err = readSSE(response.Body, d.receive)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func wireError(message string) error {
	return protocol.E("model_protocol", "Chat Completions: "+message)
}
