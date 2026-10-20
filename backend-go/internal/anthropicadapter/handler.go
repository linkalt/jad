package anthropicadapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Handler serves the Responses API routes.
type Handler struct {
	config *Config
	client anthropic.Client
	now    func() time.Time
	logger *slog.Logger
}

// NewHandler builds the HTTP handler. A nil client uses a plain http.Client;
// the request timeout is applied per request through the context so a long
// stream is not cut by a client-wide deadline.
func NewHandler(config *Config, httpClient *http.Client) *Handler {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	handler := &Handler{config: config, client: newAnthropicClient(config, httpClient), now: time.Now}
	handler.SetLogger(nil)
	return handler
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
	params, apiErr := sdkMessageParams(h.config, &translated.Request)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	var upstream *http.Response
	message, err := h.client.Messages.New(ctx, params, h.sdkRequestOptions(apiKey, &upstream)...)
	defer closeHTTPResponse(upstream)
	if err != nil {
		if status, header, payload, ok := sdkHTTPError(err, upstream); ok {
			h.logUpstreamFailure(upstreamFailure{Kind: "http_status", Status: status, Model: translated.Request.Model,
				Header: header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
			writeError(w, upstreamError(status, payload, header))
			return
		}
		writeError(w, sdkCallError(err, upstream))
		return
	}
	if message == nil {
		writeError(w, &apiError{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_invalid_body",
			Message: "the upstream response is empty"})
		return
	}
	payload := []byte(message.RawJSON())
	if len(payload) == 0 {
		payload, err = json.Marshal(message)
		if err != nil {
			writeError(w, invalidUpstreamBody(err))
			return
		}
	}
	var response anthropicResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		writeError(w, invalidUpstreamBody(err))
		return
	}
	// A non-streaming error can still arrive as {"type":"error"} with status 200.
	if response.Type == "error" {
		var header http.Header
		var status int
		if upstream != nil {
			header, status = upstream.Header, upstream.StatusCode
		}
		h.logUpstreamFailure(upstreamFailure{Kind: "error_body_with_200", Status: status, Model: translated.Request.Model,
			Header: header, Body: string(payload), SentHeaders: sentHeaderNames(h.upstreamHeaders(""))})
		writeError(w, upstreamError(http.StatusBadGateway, payload, header))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if upstream != nil {
		if id := upstream.Header.Get("request-id"); id != "" {
			w.Header().Set("request-id", id)
		}
	}
	_ = json.NewEncoder(w).Encode(translateResponse(&response, h.now()))
}

// upstreamHeaders returns the adapter-configured upstream headers. The SDK adds its own standard
// headers; this set is also used to record request header names without recording credential values.
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
