package anthropicadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// messagesPath is the Messages API route, resolved against upstream.base_url.
const messagesPath = "v1/messages"

// defaultUserAgent names the adapter to the upstream; upstream.extra_headers can replace it.
const defaultUserAgent = "jad-anthropic-adapter"

// messagesURL resolves /v1/messages against the base URL the way a relative reference
// resolves against a directory: a base path is kept, with or without a trailing slash
// (https://host/api becomes https://host/api/v1/messages).
func messagesURL(baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if base.Path != "" && base.Path[len(base.Path)-1] != '/' {
		base.Path += "/"
		base.RawPath = ""
	}
	resolved, err := base.Parse(messagesPath)
	if err != nil {
		return "", err
	}
	return resolved.String(), nil
}

// sendMessages posts one Messages request and returns the response with its body unread,
// whatever its status. The request is sent once: upstream.stream_retry is the only retry,
// so the adapter can inspect every answer before anything is tried again. ctx carries the
// request timeout.
func (h *Handler) sendMessages(ctx context.Context, apiKey string, payload []byte, stream bool) (*http.Response, error) {
	if h.messagesURL == "" {
		return nil, fmt.Errorf("upstream.base_url %q cannot be resolved", h.config.Upstream.BaseURL)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.messagesURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header = h.upstreamHeaders(apiKey)
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", defaultUserAgent)
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}
	return h.client.Do(request)
}

// messagesPayload is the JSON body of the Messages request: the adapter's own translation,
// with the matched profile's extra_body merged in.
func messagesPayload(config *Config, request *anthropicRequest) ([]byte, *apiError) {
	payload, err := encodeRequestBody(config, request)
	if err != nil {
		return nil, &apiError{
			Status: http.StatusInternalServerError, Type: "api_error", Code: "encode_failed", Message: err.Error(),
		}
	}
	return payload, nil
}

// encodeRequestBody marshals the request and merges the matched profile's extra_body into it. With no extra
// body the bytes are exactly the marshalled request; raw values are kept as written.
func encodeRequestBody(config *Config, request *anthropicRequest) ([]byte, error) {
	payload, err := json.Marshal(request)
	extra := config.ProfileFor(request.Model).ExtraBody
	if err != nil || len(extra) == 0 {
		return payload, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	for name, value := range extra {
		fields[name] = value
	}
	return json.Marshal(fields)
}

// upstreamFailed reports whether a response carries an HTTP error status.
func upstreamFailed(response *http.Response) bool {
	return response.StatusCode < http.StatusOK || response.StatusCode > 299
}

// readErrorBody reads at most errorBodyLimit bytes of an upstream error body.
func readErrorBody(response *http.Response) []byte {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
	return payload
}

// callError maps a failure to send the request or to read a successful response.
// response is nil when no HTTP answer arrived.
func callError(err error, response *http.Response) *apiError {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return invalidUpstreamBody(err)
	}

	status := http.StatusBadGateway
	code := "upstream_unreachable"
	message := "calling the Anthropic API failed: " + err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	} else if response != nil && !upstreamFailed(response) {
		code = "upstream_read_failed"
		message = "reading the upstream response failed: " + err.Error()
	}
	return &apiError{Status: status, Type: "api_error", Code: code, Message: message}
}

func invalidUpstreamBody(err error) *apiError {
	return &apiError{
		Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_invalid_body",
		Message: "the upstream response is not valid JSON: " + err.Error(),
	}
}

func closeHTTPResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
