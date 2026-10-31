package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	maxRemoteAPIHops      = 16
	maxRemoteAPIRuns      = 10_000
	maxRemoteAPIRetention = 24 * 60 * 60 * 1000
	maxRemoteAPIWait      = 60_000
	maxRemoteAPIBody      = 16 << 20
	maxRemoteAPIResult    = 16 << 20
)

var remoteAPIKeys = []string{"enabled", "tokens_file", "listen_unix", "listen_unix_mode", "max_hops", "max_runs", "max_runs_per_token", "run_timeout_ms", "run_retention_ms", "wait_max_ms", "body_max_bytes", "result_max_bytes"}

// removedRemoteAPIKeys are keys of the https era. They are refused by name with the reason, so
// a user never keeps a certificate believing the remote API is still behind TLS.
var removedRemoteAPIKeys = []string{"tls_cert_file", "tls_key_file"}

// removedRemoteAPIHint replaces the removed TLS settings.
const removedRemoteAPIHint = "the remote routes are served on the plain http listener now, where the bearer token travels in clear text: put that listener behind a VPN or serve listen_unix instead; delete the key"

// RemoteAPISettings is server.remote_api: the /api/remote/v1 routes other JAD servers call.
// They are served on ListenUnix when it is set, which keeps them off the network, and on the
// main listener, which is plain http, when it is empty.
type RemoteAPISettings struct {
	Enabled bool
	// TokensFile is the absolute token table; required when Enabled.
	TokensFile string
	// ListenUnix is the absolute socket path; empty serves no socket. ListenUnixMode is its permission.
	ListenUnix     string
	ListenUnixMode uint32
	MaxHops        int
	MaxRuns        int
	// MaxRunsPerToken bounds the live runs one token may have; finished runs do not count.
	MaxRunsPerToken int
	// RunTimeoutMs bounds a message run started by the remote API (queue time excluded).
	RunTimeoutMs   int64
	RunRetentionMs int64
	WaitMaxMs      int64
	BodyMaxBytes   int64
	ResultMaxBytes int64
}

func normalizeRemoteAPI(raw json.RawMessage, path, base string) (RemoteAPISettings, error) {
	settings := RemoteAPISettings{MaxHops: 3, MaxRuns: 256, MaxRunsPerToken: 16, RunTimeoutMs: 3_600_000, RunRetentionMs: 10 * 60 * 1000, WaitMaxMs: 25_000, BodyMaxBytes: 1 << 20, ResultMaxBytes: 256 << 10, ListenUnixMode: 0o600}
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api must be an object", path)
	}
	for key := range fields {
		if containsString(removedRemoteAPIKeys, key) {
			return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.%s was removed; %s", path, key, removedRemoteAPIHint)
		}
		if !containsString(remoteAPIKeys, key) {
			return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.%s is not a setting (%s)", path, key, strings.Join(remoteAPIKeys, ", "))
		}
	}
	text := func(name string, target *string) error {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return nil
		}
		var parsed string
		if json.Unmarshal(value, &parsed) != nil || strings.TrimSpace(parsed) == "" {
			return fmt.Errorf("%s: server.remote_api.%s must be a non-empty string", path, name)
		}
		*target = strings.TrimSpace(parsed)
		return nil
	}
	number := func(name string, target *int64, min, max int64) error {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return nil
		}
		parsed, valid := configSafeInteger(value)
		if !valid || parsed < min || parsed > max {
			return fmt.Errorf("%s: server.remote_api.%s must be an integer between %d and %d", path, name, min, max)
		}
		*target = parsed
		return nil
	}
	if value, ok := fields["enabled"]; ok && string(value) != "null" {
		if json.Unmarshal(value, &settings.Enabled) != nil {
			return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.enabled must be a boolean", path)
		}
	}
	for _, field := range []struct {
		name   string
		target *string
	}{{"tokens_file", &settings.TokensFile}, {"listen_unix", &settings.ListenUnix}} {
		if err := text(field.name, field.target); err != nil {
			return RemoteAPISettings{}, err
		}
	}
	if settings.TokensFile != "" {
		settings.TokensFile = resolveFrom(base, settings.TokensFile)
	}
	hops, runs, perToken, mode := int64(settings.MaxHops), int64(settings.MaxRuns), int64(settings.MaxRunsPerToken), int64(settings.ListenUnixMode)
	for _, field := range []struct {
		name     string
		target   *int64
		min, max int64
	}{
		{"max_hops", &hops, 1, maxRemoteAPIHops}, {"max_runs", &runs, 1, maxRemoteAPIRuns}, {"max_runs_per_token", &perToken, 1, maxRemoteAPIRuns},
		{"run_timeout_ms", &settings.RunTimeoutMs, 1000, 6 * 60 * 60 * 1000}, {"run_retention_ms", &settings.RunRetentionMs, 1000, maxRemoteAPIRetention}, {"wait_max_ms", &settings.WaitMaxMs, 1, maxRemoteAPIWait},
		{"body_max_bytes", &settings.BodyMaxBytes, 1, maxRemoteAPIBody}, {"result_max_bytes", &settings.ResultMaxBytes, 1, maxRemoteAPIResult},
		{"listen_unix_mode", &mode, 0o600, 0o666},
	} {
		if err := number(field.name, field.target, field.min, field.max); err != nil {
			return RemoteAPISettings{}, err
		}
	}
	settings.MaxHops, settings.MaxRuns, settings.MaxRunsPerToken, settings.ListenUnixMode = int(hops), int(runs), int(perToken), uint32(mode)
	// The socket's mode is its permission: a group or other write bit lets another user
	// write to it, so the range check above is not enough on its own.
	if settings.ListenUnixMode&0o022 != 0 {
		return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.listen_unix_mode must not grant group or other write access", path)
	}
	if !settings.Enabled {
		return settings, nil
	}
	switch {
	case settings.TokensFile == "":
		return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.tokens_file is required when it is enabled", path)
	case settings.ListenUnix != "" && !filepath.IsAbs(settings.ListenUnix):
		return RemoteAPISettings{}, fmt.Errorf("%s: server.remote_api.listen_unix must be an absolute path", path)
	}
	return settings, nil
}
