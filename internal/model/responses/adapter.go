package responses

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

// Config contains a credential resolver, never a key in persisted request data.
// Endpoint is the API base URL (for example https://api.openai.com/v1).
type Config struct {
	Endpoint   string
	Effort     string
	Credential func(context.Context) (string, error)
	Client     *http.Client
	Images     bool
	// ClientToolSearch requires an explicitly confirmed endpoint/model and a
	// host that retains the executors returned by each discovery result.
	ClientToolSearch bool
}

type Adapter struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config) (*Adapter, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, protocol.E("invalid_config", "Responses endpoint must be an HTTP API base URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	var client http.Client
	if config.Client != nil {
		client = *config.Client
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		client.Transport = transport
	}
	// Never forward authorization to a redirect target, even for a custom client.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Adapter{config: config, endpoint: u.String(), client: &client}, nil
}

func (a *Adapter) Stream(ctx context.Context, request model.Request, emit func(model.Delta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.config.Images {
		for _, message := range request.Messages {
			for _, block := range message.Content {
				if block.Type == "image" {
					return protocol.E("unsupported_capability", "selected Responses model does not declare image input")
				}
			}
		}
	}
	body, err := encodeSearchRequest(request, a.endpoint, a.config.Effort, false, a.config.ClientToolSearch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/responses", bytes.NewReader(body))
	if err != nil {
		return protocol.E("invalid_config", "Responses request could not be constructed")
	}
	if a.config.Credential != nil {
		key, err := a.config.Credential(ctx)
		if err != nil {
			return protocol.E("credential_unavailable", "Responses credential could not be resolved")
		}
		if strings.ContainsAny(key, "\r\n") {
			return protocol.E("credential_unavailable", "Responses credential is invalid")
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if len(request.ID) <= 128 && request.ID != "" && !strings.ContainsAny(request.ID, "\r\n") {
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
		return protocol.E("model_transport", "Responses connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Provider bodies and URLs can echo secrets or prompts. Keep them out of events.
		return model.HTTPFailure("Responses", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return wireError("expected text/event-stream")
	}
	d := decoder{endpoint: a.endpoint, model: request.Model, emit: emit, clientSearch: a.config.ClientToolSearch}
	if a.config.ClientToolSearch {
		for _, tool := range request.Tools {
			d.searchAdvertised = d.searchAdvertised || tool.Name == searchToolName
		}
	}
	err = readSSE(response.Body, d.receive)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (a *Adapter) ValidateRequest(request model.Request) error {
	return a.validate(request, false)
}

func (a *Adapter) ValidateContext(request model.Request) error {
	return a.validate(request, true)
}

func (a *Adapter) validate(request model.Request, allowPending bool) error {
	if !a.config.Images {
		for _, message := range request.Messages {
			for _, block := range message.Content {
				if block.Type == "image" {
					return protocol.E("unsupported_capability", "selected Responses model does not declare image input")
				}
			}
		}
	}
	_, err := encodeSearchRequest(request, a.endpoint, a.config.Effort, allowPending, a.config.ClientToolSearch)
	return err
}
