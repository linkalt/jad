package anthropicadapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// apiError is an error the adapter reports in the OpenAI error envelope.
type apiError struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
	// RetryAfter is copied from the upstream Retry-After header when set.
	RetryAfter string
}

func (e *apiError) Error() string { return e.Message }

func badRequest(param, format string, args ...any) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Type: "invalid_request_error",
		Code: "invalid_request", Param: param, Message: fmt.Sprintf(format, args...),
	}
}

func unsupported(param, format string, args ...any) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Type: "invalid_request_error",
		Code: "unsupported_parameter", Param: param, Message: fmt.Sprintf(format, args...),
	}
}

func notImplemented(format string, args ...any) *apiError {
	return &apiError{
		Status: http.StatusNotImplemented, Type: "invalid_request_error",
		Code: "unsupported_operation", Message: fmt.Sprintf(format, args...),
	}
}

func unauthorized(message string) *apiError {
	return &apiError{
		Status: http.StatusUnauthorized, Type: "authentication_error",
		Code: "invalid_api_key", Message: message,
	}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func (e *apiError) envelope() errorEnvelope {
	body := errorBody{Message: e.Message, Type: e.Type, Code: e.Code}
	if e.Param != "" {
		param := e.Param
		body.Param = &param
	}
	return errorEnvelope{Error: body}
}

func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter != "" {
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e.envelope())
}

// anthropicErrorBody is the Anthropic error shape, used for an HTTP error body
// and for an in-stream error event alike.
type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// upstreamError converts a non-2xx Anthropic response. The status is kept so
// the caller's retry policy (429, 5xx, 529) keeps working through the adapter.
func upstreamError(status int, body []byte, header http.Header) *apiError {
	var parsed anthropicErrorBody
	_ = json.Unmarshal(body, &parsed)
	kind := parsed.Error.Type
	if kind == "" {
		kind = "api_error"
	}
	message := parsed.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
		if message == "" {
			message = fmt.Sprintf("upstream returned HTTP %d", status)
		}
	}
	return &apiError{
		Status: status, Type: kind, Code: kind, Message: message,
		RetryAfter: header.Get("Retry-After"),
	}
}
