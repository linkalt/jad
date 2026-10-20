package anthropicadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func newAnthropicClient(config *Config, httpClient *http.Client) anthropic.Client {
	return anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(config.Upstream.BaseURL),
		option.WithHTTPClient(httpClient),
		// Adapter stream_retry is the only retry policy; the SDK must not retry
		// an upstream request invisibly before the adapter can inspect it.
		option.WithMaxRetries(0),
	)
}

func sdkMessageParams(config *Config, request *anthropicRequest) (anthropic.MessageNewParams, *apiError) {
	payload, err := encodeRequestBody(config, request)
	if err != nil {
		return anthropic.MessageNewParams{}, &apiError{
			Status: http.StatusInternalServerError, Type: "api_error", Code: "encode_failed", Message: err.Error(),
		}
	}

	// Keep the adapter's existing wire translation intact, including fields and
	// union variants that may not yet have a generated SDK field. The SDK needs
	// Model and MaxTokens for its request setup and timeout calculation; SetJSON
	// uses the original payload for the actual request body.
	params := anthropic.MessageNewParams{Model: request.Model, MaxTokens: int64(request.MaxTokens)}
	param.SetJSON(payload, &params)
	return params, nil
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

func (h *Handler) sdkRequestOptions(apiKey string, response **http.Response) []option.RequestOption {
	options := []option.RequestOption{
		option.WithResponseInto(response),
		option.WithRequestTimeout(time.Duration(h.config.Upstream.RequestTimeoutMS) * time.Millisecond),
	}
	headers := h.upstreamHeaders(apiKey)
	if h.config.Upstream.AuthHeader == AuthHeaderXAPIKey {
		options = append(options, option.WithAPIKey(apiKey))
		headers.Del("X-Api-Key")
	} else {
		options = append(options, option.WithAuthToken(apiKey))
		headers.Del("Authorization")
	}
	for name, values := range headers {
		for _, value := range values {
			options = append(options, option.WithHeader(name, value))
		}
	}
	return options
}

func sdkHTTPError(err error, response *http.Response) (int, http.Header, []byte, bool) {
	var sdkErr *anthropic.Error
	if errors.As(err, &sdkErr) {
		if response == nil || response.StatusCode == 0 {
			response = sdkErr.Response
		}
		status := sdkErr.StatusCode
		var header http.Header
		if response != nil {
			if response.StatusCode != 0 {
				status = response.StatusCode
			}
			header = response.Header
		}
		if status != 0 && (status < http.StatusOK || status > 299) {
			return status, header, []byte(sdkErr.RawJSON()), true
		}
	}
	if response != nil && response.StatusCode != 0 && (response.StatusCode < http.StatusOK || response.StatusCode > 299) {
		return response.StatusCode, response.Header, nil, true
	}
	return 0, nil, nil, false
}

func sdkCallError(err error, response *http.Response) *apiError {
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
	} else if response != nil && response.StatusCode >= http.StatusOK && response.StatusCode <= 299 {
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
