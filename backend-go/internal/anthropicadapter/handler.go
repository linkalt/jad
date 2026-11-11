package anthropicadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler serves the Responses API routes.
type Handler struct {
	config *Config
	client *http.Client
	// messagesURL is upstream.base_url with /v1/messages resolved against it.
	messagesURL string
	now         func() time.Time
	logger      *slog.Logger
}

// NewHandler builds the HTTP handler. A nil client uses a plain http.Client;
// the request timeout is applied per request through the context so a long
// stream is not cut by a client-wide deadline.
func NewHandler(config *Config, httpClient *http.Client) *Handler {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// The config was validated, so the URL parses; an empty one fails each request instead.
	endpoint, _ := messagesURL(config.Upstream.BaseURL)
	handler := &Handler{config: config, client: httpClient, messagesURL: endpoint, now: time.Now}
	handler.SetLogger(nil)
	return handler
}

// ServeHTTP routes /v1/responses and /responses.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Browsers always send Origin on a cross-origin POST; the API clients this adapter serves never do.
	// Only checked when the adapter spends its own credential.
	if h.config.HoldsCredential() && r.Header.Get("Origin") != "" {
		writeError(w, &apiError{Status: http.StatusForbidden, Type: "invalid_request_error",
			Code: "origin_not_allowed", Message: "browser cross-origin requests are not accepted"})
		return
	}
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
	apiKey, credentialErr := h.upstreamToken(r)
	if credentialErr != nil {
		writeError(w, credentialErr)
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
		h.respondStream(w, r, apiKey, translated)
		return
	}
	h.respond(w, r, apiKey, translated)
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, apiKey string, translated *translation) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.config.Upstream.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	body, apiErr := messagesPayload(h.config, &translated.Request)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	upstream, err := h.sendMessages(ctx, apiKey, body, false)
	if err != nil {
		writeError(w, callError(err, nil))
		return
	}
	defer closeHTTPResponse(upstream)
	if upstreamFailed(upstream) {
		payload := readErrorBody(upstream)
		h.logUpstreamFailure(upstreamFailure{Kind: "http_status", Status: upstream.StatusCode, Model: translated.Request.Model,
			Header: upstream.Header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
		writeError(w, upstreamError(upstream.StatusCode, payload, upstream.Header))
		return
	}
	payload, err := io.ReadAll(upstream.Body)
	if err != nil {
		writeError(w, callError(err, upstream))
		return
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		writeError(w, &apiError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_invalid_body",
			Message: "the upstream response is empty"})
		return
	}
	var response anthropicResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		writeError(w, invalidUpstreamBody(err))
		return
	}
	// A non-streaming error can still arrive as {"type":"error"} with status 200.
	if response.Type == "error" {
		h.logUpstreamFailure(upstreamFailure{Kind: "error_body_with_200", Status: upstream.StatusCode, Model: translated.Request.Model,
			Header: upstream.Header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
		writeError(w, upstreamError(http.StatusBadGateway, payload, upstream.Header))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if id := upstream.Header.Get("request-id"); id != "" {
		w.Header().Set("request-id", id)
	}
	_ = json.NewEncoder(w).Encode(translateResponse(&response, h.now()))
}

// upstreamHeaders returns the headers of an upstream request, apart from Accept and the
// default User-Agent; this set is also used to record request header names without
// recording credential values.
func (h *Handler) upstreamHeaders(apiKey string) http.Header {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if h.config.Upstream.AuthHeader == AuthHeaderXAPIKey {
		header.Set("x-api-key", apiKey)
	} else {
		header.Set("Authorization", "Bearer "+apiKey)
	}
	header.Set("anthropic-version", h.config.Upstream.AnthropicVersion)
	if len(h.config.Upstream.Beta) > 0 {
		header.Set("anthropic-beta", strings.Join(h.config.Upstream.Beta, ","))
	}
	for name, value := range h.config.Upstream.ExtraHeaders {
		header.Set(name, value)
	}
	return header
}

// apiKeyFrom reads the key the caller sent: a Bearer token, which is what the
// OpenAI SDK uses, or an x-api-key header.
func apiKeyFrom(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
