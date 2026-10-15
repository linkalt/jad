package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeProviderStreamSettings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr string
	}{
		{name: "absent key takes the default", raw: "", want: defaultProviderStreamIdleTimeoutMS},
		{name: "empty object takes the default", raw: `{}`, want: defaultProviderStreamIdleTimeoutMS},
		{name: "explicit value", raw: `{"idle_timeout_ms": 45000}`, want: 45000},
		{name: "zero turns the guard off", raw: `{"idle_timeout_ms": 0}`, want: 0},
		{name: "lower bound", raw: `{"idle_timeout_ms": 1000}`, want: 1000},
		{name: "upper bound", raw: `{"idle_timeout_ms": 3600000}`, want: 3600000},
		{name: "below the lower bound", raw: `{"idle_timeout_ms": 999}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "above the upper bound", raw: `{"idle_timeout_ms": 3600001}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "negative", raw: `{"idle_timeout_ms": -1}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "fraction", raw: `{"idle_timeout_ms": 1500.5}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "string", raw: `{"idle_timeout_ms": "1000"}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "null value does not pick a default", raw: `{"idle_timeout_ms": null}`, wantErr: "provider_stream.idle_timeout_ms"},
		{name: "unknown key", raw: `{"idle_timeout": 5000}`, wantErr: "idle_timeout"},
		{name: "not an object", raw: `5000`, wantErr: "provider_stream must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProviderStreamSettings(json.RawMessage(tc.raw), "session.json")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.IdleTimeoutMS != tc.want {
				t.Fatalf("IdleTimeoutMS = %d, want %d", got.IdleTimeoutMS, tc.want)
			}
		})
	}
}

// A layout session, profile and the root defaults may all carry provider_stream; the
// nearer level wins, and an invalid value is refused wherever it comes from.
func TestLayoutProviderStreamFromDefaultsSessionAndProfile(t *testing.T) {
	layout, location, root := sampleLayout(t)
	load := func() (SessionConfig, error) {
		return LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	}
	config, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if config.ProviderStream.IdleTimeoutMS != defaultProviderStreamIdleTimeoutMS {
		t.Fatalf("unset: %d, want the default", config.ProviderStream.IdleTimeoutMS)
	}
	layout.Defaults = map[string]json.RawMessage{"provider_stream": json.RawMessage(`{"idle_timeout_ms":11000}`)}
	if config, err = load(); err != nil || config.ProviderStream.IdleTimeoutMS != 11000 {
		t.Fatalf("defaults: %v, %d, want 11000", err, config.ProviderStream.IdleTimeoutMS)
	}
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"provider_stream": map[string]any{"idle_timeout_ms": 0}}
	})
	if config, err = load(); err != nil || config.ProviderStream.IdleTimeoutMS != 0 {
		t.Fatalf("profile: %v, %d, want 0 (off) to beat the defaults", err, config.ProviderStream.IdleTimeoutMS)
	}
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"provider_stream": map[string]any{"idle_timeout_ms": 5}}
	})
	if _, err = load(); err == nil || !strings.Contains(err.Error(), "provider_stream.idle_timeout_ms") {
		t.Fatalf("error = %v, want the invalid profile value refused", err)
	}
}

func TestLayoutRefusesMaxConcurrentRunsInSettings(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	layout.Defaults = map[string]json.RawMessage{"max_concurrent_runs": json.RawMessage(`4`)}
	_, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	if err == nil || !strings.Contains(err.Error(), "max_concurrent_runs") {
		t.Fatalf("error = %v, want server.max_concurrent_runs refused as a session setting", err)
	}
}
