// Package gemini adapts streamGenerateContent, not the Interactions or Live API.
package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

const Protocol = "gemini"

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

type Config struct {
	Endpoint, Effort        string
	ThinkingBudget          *int
	IncludeThoughts, Images bool
	Credential              func(context.Context) (string, error)
	Client                  *http.Client
}

type Adapter struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config) (*Adapter, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, protocol.E("invalid_config", "Gemini endpoint must be an HTTP API base URL without credentials, query or fragment")
	}
	config.Effort = strings.ToUpper(config.Effort)
	switch config.Effort {
	case "", "MINIMAL", "LOW", "MEDIUM", "HIGH":
	default:
		return nil, protocol.E("invalid_config", "unsupported Gemini thinking level")
	}
	if config.ThinkingBudget != nil {
		if *config.ThinkingBudget < -1 || config.Effort != "" {
			return nil, protocol.E("invalid_config", "Gemini thinking budget must be at least -1 and cannot be combined with a thinking level")
		}
		budget := *config.ThinkingBudget
		config.ThinkingBudget = &budget
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
	name := strings.TrimPrefix(request.Model, "models/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/models/"+url.PathEscape(name)+":streamGenerateContent?alt=sse", bytes.NewReader(body))
	if err != nil {
		return protocol.E("invalid_config", "Gemini request could not be constructed")
	}
	if a.config.Credential != nil {
		key, err := a.config.Credential(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || strings.ContainsAny(key, "\r\n") {
			return protocol.E("credential_unavailable", "Gemini credential could not be resolved")
		}
		if key != "" {
			req.Header.Set("x-goog-api-key", key)
		}
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
		return protocol.E("model_transport", "Gemini connection failed")
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
		return protocol.E(kind, fmt.Sprintf("Gemini HTTP %d", response.StatusCode))
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return wireError("expected text/event-stream")
	}
	seed := request.ID
	if seed == "" {
		seed = protocol.NewID("gemini")
	}
	digest := sha256.Sum256([]byte(seed))
	d := decoder{endpoint: a.endpoint, model: request.Model, prefix: "gemini_" + hex.EncodeToString(digest[:16]), emit: emit}
	err = readSSE(response.Body, d.receive)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return d.complete()
}

func wireError(message string) error { return protocol.E("model_protocol", "Gemini: "+message) }
