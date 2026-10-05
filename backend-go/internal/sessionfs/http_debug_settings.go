package sessionfs

import (
	"encoding/json"
	"math"
	"strings"
)

const (
	defaultHTTPDebugBodyBytes int64 = 64 * 1024
	maxHTTPDebugBodyBytes     int64 = 10 * 1024 * 1024
)

// HTTPDebugSettings controls opt-in capture of provider HTTP requests and
// responses. It contains no credentials; the transport redacts sensitive
// request headers before writing diagnostics.
type HTTPDebugSettings struct {
	Enabled                bool  `json:"enabled"`
	IncludeRequestHeaders  bool  `json:"include_request_headers"`
	IncludeRequestBody     bool  `json:"include_request_body"`
	IncludeResponseHeaders bool  `json:"include_response_headers"`
	IncludeResponseBody    bool  `json:"include_response_body"`
	MaxBodyBytes           int64 `json:"max_body_bytes"`
}

func defaultHTTPDebugSettings() HTTPDebugSettings {
	return HTTPDebugSettings{
		IncludeResponseHeaders: true,
		MaxBodyBytes:           defaultHTTPDebugBodyBytes,
	}
}

func normalizeHTTPDebugSettings(raw json.RawMessage, path string) (HTTPDebugSettings, error) {
	settings := defaultHTTPDebugSettings()
	if len(raw) == 0 {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return HTTPDebugSettings{}, configError("%s: http_debug must be a JSON object", path)
	}
	booleanFields := []struct {
		name   string
		target *bool
	}{
		{"enabled", &settings.Enabled},
		{"include_request_headers", &settings.IncludeRequestHeaders},
		{"include_request_body", &settings.IncludeRequestBody},
		{"include_response_headers", &settings.IncludeResponseHeaders},
		{"include_response_body", &settings.IncludeResponseBody},
	}
	for _, field := range booleanFields {
		value, exists := fields[field.name]
		if !exists {
			continue
		}
		if len(value) == 0 || strings.TrimSpace(string(value)) == "null" || json.Unmarshal(value, field.target) != nil {
			return HTTPDebugSettings{}, configError("%s: http_debug.%s must be a boolean", path, field.name)
		}
	}
	if value, exists := fields["max_body_bytes"]; exists && strings.TrimSpace(string(value)) != "null" {
		var number float64
		if err := json.Unmarshal(value, &number); err != nil || math.Trunc(number) != number || number < 1 || number > float64(maxHTTPDebugBodyBytes) {
			return HTTPDebugSettings{}, configError("%s: http_debug.max_body_bytes must be a safe integer between 1 and 10485760", path)
		}
		settings.MaxBodyBytes = int64(number)
	}
	return settings, nil
}
