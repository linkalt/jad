package chatadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// errorBodyLimit bounds how much of an upstream error body is read.
const errorBodyLimit = 1 << 20

// apiError is an error reported in the OpenAI error envelope, before any stream byte.
type apiError struct {
	Status     int
	Type       string
	Code       string
	Param      string
	Message    string
	RetryAfter string
}

func badRequest(param, format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_request", Param: param, Message: fmt.Sprintf(format, args...)}
}

// upstreamBody is the error object of a Chat Completions error answer.
type upstreamBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// Handler serves POST /responses (and /v1/responses) by calling {base_url}/chat/completions.
type Handler struct {
	config *Config
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
}

// NewHandler builds the handler. A nil client uses a plain http.Client and a nil logger
// discards. The request timeout is applied per request, so a long stream is not cut by a
// client-wide deadline.
func NewHandler(config *Config, client *http.Client, logger *slog.Logger) *Handler {
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{config: config, client: client, logger: logger, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/v1")
	if path != "/responses" || r.Method != http.MethodPost {
		writeError(w, &apiError{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "not_found", Message: "only POST /responses is served"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.config.MaxRequestBytes+1))
	if err != nil {
		writeError(w, badRequest("", "reading the request body failed: %v", err))
		return
	}
	if int64(len(body)) > h.config.MaxRequestBytes {
		writeError(w, &apiError{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large", Message: "request body exceeds max_request_bytes"})
		return
	}
	payload, model, apiErr := translateRequest(h.config, body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.config.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeError(w, &apiError{Status: http.StatusInternalServerError, Type: "api_error", Code: "request_failed", Message: err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		upstream.Header.Set("Authorization", authorization)
	}
	for name, value := range h.config.ExtraHeaders {
		upstream.Header.Set(name, value)
	}
	response, err := h.client.Do(upstream)
	if err != nil {
		status, code := http.StatusBadGateway, "upstream_unreachable"
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, &apiError{Status: status, Type: "api_error", Code: code, Message: "calling the Chat Completions API failed: " + err.Error()})
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
		h.logger.Warn("chat completions upstream failed", "status", response.StatusCode, "model", model, "request_id", response.Header.Get("x-request-id"))
		writeError(w, upstreamError(response.StatusCode, data, response.Header))
		return
	}
	sink := newSink(w)
	translator := newTranslator(sink, h.config, model, h.now)
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		translator.run(response.Body)
		return
	}
	// A server that ignores stream:true answers with one JSON completion; it is
	// translated as a single chunk.
	data, err := io.ReadAll(io.LimitReader(response.Body, h.config.MaxEventBytes+1))
	if err != nil || int64(len(data)) > h.config.MaxEventBytes {
		translator.fail("upstream_read_failed", "reading the upstream completion failed")
		return
	}
	var completion chatChunk
	if err := json.Unmarshal(data, &completion); err != nil {
		translator.fail("upstream_invalid_body", "the upstream answer is neither an event stream nor a JSON completion")
		return
	}
	translator.onChunk(&completion)
	if !translator.done {
		translator.complete()
	}
}

// upstreamError keeps the upstream status and Retry-After, so the Responses client's
// retry sees what the provider said.
func upstreamError(status int, data []byte, header http.Header) *apiError {
	apiErr := &apiError{Status: status, Type: "api_error", Code: "upstream_error", RetryAfter: header.Get("Retry-After")}
	var envelope struct {
		Error *upstreamBody `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		apiErr.Message = envelope.Error.Message
		apiErr.Code = errorCode(envelope.Error)
		if envelope.Error.Type != "" {
			apiErr.Type = envelope.Error.Type
		}
		return apiErr
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 512 {
		text = text[:512]
	}
	if text == "" {
		text = http.StatusText(status)
	}
	apiErr.Message = fmt.Sprintf("the Chat Completions API answered HTTP %d: %s", status, text)
	return apiErr
}

func writeError(w http.ResponseWriter, apiErr *apiError) {
	if apiErr.RetryAfter != "" {
		w.Header().Set("Retry-After", apiErr.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.Status)
	var param *string
	if apiErr.Param != "" {
		param = &apiErr.Param
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": apiErr.Message, "type": apiErr.Type, "param": param, "code": apiErr.Code,
	}})
}
