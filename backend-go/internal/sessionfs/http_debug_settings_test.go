package sessionfs

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeHTTPDebugSettingsDefaultsAndOverrides(t *testing.T) {
	defaults, err := normalizeHTTPDebugSettings(nil, "setting.json")
	if err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	if want := (HTTPDebugSettings{IncludeResponseHeaders: true, MaxBodyBytes: 64 * 1024, MaxTotalBytes: 256 * 1024 * 1024}); !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %+v, want %+v", defaults, want)
	}

	override, err := normalizeHTTPDebugSettings(json.RawMessage(`{"enabled":true,"include_request_headers":true,"include_request_body":true,"include_response_headers":false,"include_response_body":true,"max_body_bytes":1024,"max_total_bytes":0,"future_option":"kept-by-TS"}`), "setting.json")
	if err != nil {
		t.Fatalf("normalize overrides: %v", err)
	}
	if want := (HTTPDebugSettings{Enabled: true, IncludeRequestHeaders: true, IncludeRequestBody: true, IncludeResponseBody: true, MaxBodyBytes: 1024}); !reflect.DeepEqual(override, want) {
		t.Fatalf("override = %+v, want %+v", override, want)
	}
}

func TestNormalizeHTTPDebugSettingsRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "not object", raw: `"enabled"`},
		{name: "boolean null", raw: `{"enabled":null}`},
		{name: "boolean number", raw: `{"include_request_body":1}`},
		{name: "fractional bytes", raw: `{"max_body_bytes":1.5}`},
		{name: "negative total", raw: `{"max_total_bytes":-1}`},
		{name: "fractional total", raw: `{"max_total_bytes":1.5}`},
		{name: "too large total", raw: `{"max_total_bytes":1099511627777}`},
		{name: "zero bytes", raw: `{"max_body_bytes":0}`},
		{name: "too many bytes", raw: `{"max_body_bytes":10485761}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeHTTPDebugSettings(json.RawMessage(test.raw), "setting.json"); err == nil {
				t.Fatalf("normalize %s unexpectedly succeeded", test.name)
			}
		})
	}
}
