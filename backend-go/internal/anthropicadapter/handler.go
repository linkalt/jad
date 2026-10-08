package anthropicadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Handler serves the Responses API routes.
type Handler struct {
	config *Config
	client *http.Client
	now    func() time.Time
}

// NewHandler builds the HTTP handler. A nil client uses a plain http.Client;
// the request timeout is applied per request through the context so a long
// stream is not cut by a client-wide deadline.
func NewHandler(config *Config, client *http.Client) *Handler {
	if client == nil {
		client = &http.Client{}
	}
	return &Handler{config: config, client: client, now: time.Now}
}

// ServeHTTP routes /v1/responses and /responses.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	path = strings.TrimPrefix(path, "/v1")
	switch {
	case path == "/responses" && r.Method == http.MethodPost:
		h.create(w, r)
	case strings.HasPrefix(path, "/responses/"):
		writeError(w, notImplemented("stored responses are not supported: the adapter keeps no state"))
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	apiKey := apiKeyFrom(r)
	if apiKey == "" {
		writeError(w, unauthorized("send the Anthropic API key as a Bearer token or x-api-key header"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.config.MaxRequestBytes+1))
	if err != nil {
		writeError(w, badRequest("", "reading the request body failed: %v", err))
		return
	}
	if int64(len(body)) > h.config.MaxRequestBytes {
		writeError(w, &apiError{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error",
			Code: "request_too_large", Message: "request body exceeds max_request_bytes"})
		return
	}
	translated, apiErr := translateRequest(h.config, body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if len(translated.Ignored) > 0 {
		w.Header().Set("X-Adapter-Ignored-Fields", strings.Join(translated.Ignored, ","))
	}
	if translated.Stream {
		writeError(w, notImplemented("streaming is not implemented yet"))
		return
	}
	h.respond(w, r, apiKey, translated)
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, apiKey string, translated *translation) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.config.Upstream.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	upstream, apiErr := h.callUpstream(ctx, apiKey, &translated.Request)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	defer upstream.Body.Close()
	payload, err := io.ReadAll(upstream.Body)
	if err != nil {
		writeError(w, &apiError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_read_failed",
			Message: "reading the upstream response failed: " + err.Error()})
		return
	}
	if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
		writeError(w, upstreamError(upstream.StatusCode, payload, upstream.Header))
		return
	}
	var message anthropicResponse
	if err := json.Unmarshal(payload, &message); err != nil {
		writeError(w, &apiError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_invalid_body",
			Message: "the upstream response is not valid JSON: " + err.Error()})
		return
	}
	// A non-streaming error can still arrive as {"type":"error"} with status 200.
	if message.Type == "error" {
		writeError(w, upstreamError(http.StatusBadGateway, payload, upstream.Header))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(translateResponse(&message, h.now()))
}

// callUpstream posts the translated request to /v1/messages.
func (h *Handler) callUpstream(ctx context.Context, apiKey string, request *anthropicRequest) (*http.Response, *apiError) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, &apiError{Status: http.StatusInternalServerError, Type: "api_error", Code: "encode_failed", Message: err.Error()}
	}
	endpoint := strings.TrimSuffix(h.config.Upstream.BaseURL, "/") + "/v1/messages"
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &apiError{Status: http.StatusInternalServerError, Type: "api_error", Code: "request_failed", Message: err.Error()}
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("x-api-key", apiKey)
	upstream.Header.Set("anthropic-version", h.config.Upstream.AnthropicVersion)
	if len(h.config.Upstream.Beta) > 0 {
		upstream.Header.Set("anthropic-beta", strings.Join(h.config.Upstream.Beta, ","))
	}
	response, err := h.client.Do(upstream)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		return nil, &apiError{Status: status, Type: "api_error", Code: "upstream_unreachable",
			Message: "calling the Anthropic API failed: " + err.Error()}
	}
	return response, nil
}

// apiKeyFrom reads the key the caller sent: a Bearer token, which is what the
// OpenAI SDK uses, or an x-api-key header.
func apiKeyFrom(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
