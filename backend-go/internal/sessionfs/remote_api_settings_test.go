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

func TestRemoteAPIEnabledNeedsTokensAndATransport(t *testing.T) {
	settings, err := remoteAPI(t, `{"enabled":true,"tokens_file":"tokens.json","listen_unix":"/run/jad.sock","listen_unix_mode":432}`)
	if err != nil || settings.TokensFile != "/etc/jad/tokens.json" || settings.ListenUnixMode != 0o660 {
		t.Fatalf("%+v %v", settings, err)
	}
	if settings, err = remoteAPI(t, `{"enabled":true,"tokens_file":"t.json","tls_cert_file":"c.pem","tls_key_file":"k.pem","max_hops":5}`); err != nil || settings.TLSCertFile != "/etc/jad/c.pem" || settings.MaxHops != 5 {
		t.Fatalf("%+v %v", settings, err)
	}
	for name, c := range map[string]struct{ body, want string }{
		"no tokens":        {`{"enabled":true,"listen_unix":"/run/x.sock"}`, "tokens_file is required"},
		"plain http":       {`{"enabled":true,"tokens_file":"t"}`, "never served over plain http"},
		"half tls":         {`{"enabled":true,"tokens_file":"t","tls_cert_file":"c"}`, "go together"},
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
