package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func remoteAPI(t *testing.T, body string) (RemoteAPISettings, error) {
	t.Helper()
	return normalizeRemoteAPI(json.RawMessage(body), "setting.json: server", "/etc/jad")
}

func TestRemoteAPIDefaultsAreOff(t *testing.T) {
	settings, err := remoteAPI(t, ``)
	if err != nil || settings.Enabled || settings.MaxHops != 3 || settings.ListenUnixMode != 0o600 {
		t.Fatalf("%+v %v", settings, err)
	}
	if settings, err = normalizeRemoteAPI(nil, "x", "/"); err != nil || settings.Enabled {
		t.Fatalf("%+v %v", settings, err)
	}
}

func TestRemoteAPIMaxRunsPerToken(t *testing.T) {
	settings, err := remoteAPI(t, ``)
	if err != nil || settings.MaxRunsPerToken != 16 {
		t.Fatalf("default: %+v %v", settings, err)
	}
	if settings, err = remoteAPI(t, `{"max_runs_per_token":4}`); err != nil || settings.MaxRunsPerToken != 4 {
		t.Fatalf("set: %+v %v", settings, err)
	}
	if _, err = remoteAPI(t, `{"max_runs_per_token":0}`); err == nil || !strings.Contains(err.Error(), "max_runs_per_token must be an integer between 1") {
		t.Fatalf("zero: %v", err)
	}
}

func TestRemoteAPIEnabledNeedsTokens(t *testing.T) {
	settings, err := remoteAPI(t, `{"enabled":true,"tokens_file":"tokens.json","listen_unix":"/run/jad.sock","listen_unix_mode":384}`)
	if err != nil || settings.TokensFile != "/etc/jad/tokens.json" || settings.ListenUnixMode != 0o600 {
		t.Fatalf("%+v %v", settings, err)
	}
	if settings, err = remoteAPI(t, `{"enabled":true,"tokens_file":"t.json","listen_unix":"/run/jad.sock","max_hops":5}`); err != nil || settings.ListenUnix != "/run/jad.sock" || settings.MaxHops != 5 {
		t.Fatalf("%+v %v", settings, err)
	}
	// The routes answer on the plain http listener, so a token table alone is enough.
	if settings, err = remoteAPI(t, `{"enabled":true,"tokens_file":"t.json"}`); err != nil || !settings.Enabled || settings.ListenUnix != "" {
		t.Fatalf("tcp only: %+v %v", settings, err)
	}
	for name, c := range map[string]struct{ body, want string }{
		"no tokens":        {`{"enabled":true,"listen_unix":"/run/x.sock"}`, "tokens_file is required"},
		"unknown key":      {`{"enabled":true,"insecure":true}`, "is not a setting"},
		"hops cap":         {`{"max_hops":99}`, "max_hops must be an integer between 1 and 16"},
		"world socket":     {`{"listen_unix_mode":511}`, "listen_unix_mode"},
		"enabled not bool": {`{"enabled":"yes"}`, "must be a boolean"},
		"not object":       {`[]`, "must be an object"},
	} {
		if _, err := remoteAPI(t, c.body); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want %q got %v", name, c.want, err)
		}
	}
	if _, err := remoteAPI(t, `{"enabled":false,"max_hops":2}`); err != nil {
		t.Fatalf("a disabled section is not validated for transports: %v", err)
	}
}

// The TLS settings of the https era are refused by name: a user who keeps them must not
// believe the remote API is still behind a certificate.
// A socket mode with a group or other write bit is refused, even inside the decimal range
// 384-438 that the schema documents: 391 is 0o607, world-writable.
func TestRemoteAPIRefusesAnyGroupOrOtherWriteBit(t *testing.T) {
	for _, mode := range []string{"391", "402", "432", "438"} {
		body := `{"enabled":true,"tokens_file":"t.json","listen_unix":"/run/jad.sock","listen_unix_mode":` + mode + `}`
		if _, err := remoteAPI(t, body); err == nil || !strings.Contains(err.Error(), "listen_unix_mode") {
			t.Fatalf("listen_unix_mode %s: error = %v, want a refusal naming listen_unix_mode", mode, err)
		}
	}
}

func TestRemoteAPIRefusesTheRemovedTLSSettingsByName(t *testing.T) {
	for _, key := range []string{"tls_cert_file", "tls_key_file"} {
		_, err := remoteAPI(t, `{"enabled":true,"tokens_file":"t.json","`+key+`":"c.pem"}`)
		if err == nil || !strings.Contains(err.Error(), "server.remote_api."+key+" was removed") || !strings.Contains(err.Error(), "plain http") {
			t.Errorf("%s: want a named removed-key error with the reason, got %v", key, err)
		}
	}
}
