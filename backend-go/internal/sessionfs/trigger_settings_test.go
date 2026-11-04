package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTriggerSettingsDefaultsAndValues(t *testing.T) {
	settings, err := normalizeServerSettings(json.RawMessage(`{}`), "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	got := settings.Triggers
	if got.Enabled || got.MaxPayloadBytes != 65536 || got.HistoryLimit != 100 || got.SignatureHeader != "X-Hub-Signature-256" || got.Entries != nil {
		t.Fatalf("defaults = %+v", got)
	}
	if defaults := Defaults().Triggers; defaults.MaxPayloadBytes != 65536 || defaults.Enabled {
		t.Fatalf("Defaults().Triggers = %+v", defaults)
	}
	settings, err = normalizeServerSettings(json.RawMessage(`{"triggers":{"enabled":true,"max_payload_bytes":1024,"history_limit":5,"signature_header":"x-signature",
		"entries":{
			"nightly":{"session":"demo","profile":"dev/coder","model":"openai/small","prompt":" Run the tests ","schedule":"30 3 * * 1-5","timezone":"UTC","timeout_ms":60000},
			"ci":{"session":"root:demo","prompt":"CI: {payload}","webhook":{"secret_env":"JAD_CI_SECRET"}},
			"off":{"enabled":false,"session":"demo","prompt":"p","schedule":"@daily"}}}}`), "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	got = settings.Triggers
	if !got.Enabled || got.MaxPayloadBytes != 1024 || got.HistoryLimit != 5 || got.SignatureHeader != "X-Signature" || len(got.Entries) != 2 {
		t.Fatalf("values = %+v", got)
	}
	ci, nightly := got.Entries[0], got.Entries[1]
	if ci.Name != "ci" || ci.Session != "root:demo" || ci.WebhookSecretEnv != "JAD_CI_SECRET" || ci.Cron != nil || ci.Timezone != "Local" {
		t.Fatalf("ci = %+v", ci)
	}
	if nightly.Name != "nightly" || nightly.Prompt != "Run the tests" || nightly.Profile != "dev/coder" || nightly.Model != "openai/small" || nightly.TimeoutMS != 60000 || nightly.Location != time.UTC || nightly.Cron == nil {
		t.Fatalf("nightly = %+v", nightly)
	}
	if next := nightly.Cron.Next(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)); !next.Equal(time.Date(2026, 11, 9, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("next after Saturday noon = %v", next)
	}
}

func TestTriggerSettingsRefusals(t *testing.T) {
	entry := func(body string) string { return `{"entries":{"t":` + body + `}}` }
	for name, test := range map[string]struct{ raw, want string }{
		"not an object":     {`[]`, "server.triggers must be an object"},
		"unknown key":       {`{"secrets":{}}`, "server.triggers.secrets is not a setting"},
		"enabled type":      {`{"enabled":1}`, "enabled must be a boolean"},
		"payload zero":      {`{"max_payload_bytes":0}`, "max_payload_bytes must be a safe integer between 1 and 1048576"},
		"history too large": {`{"history_limit":10001}`, "history_limit must be a safe integer between 1 and 10000"},
		"header with colon": {`{"signature_header":"X-Sig:"}`, "signature_header must be an HTTP header name"},
		"entries array":     {`{"entries":[]}`, "entries must be an object of named triggers"},
		"bad name":          {`{"entries":{"Bad Name":{}}}`, `"Bad Name" must be 1-64 of a-z`},
		"entry not object":  {entry(`1`), "server.triggers.entries.t must be an object"},
		"entry unknown key": {entry(`{"cron":"x"}`), "server.triggers.entries.t.cron is not a setting"},
		"no session":        {entry(`{"prompt":"p","schedule":"@daily"}`), "session must name a session"},
		"no prompt":         {entry(`{"session":"s","schedule":"@daily"}`), "prompt must be a non-empty string"},
		"neither":           {entry(`{"session":"s","prompt":"p"}`), "needs a schedule, a webhook or both"},
		"bad cron":          {entry(`{"session":"s","prompt":"p","schedule":"61 * * * *"}`), "schedule: minute: 61 is outside 0-59"},
		"bad zone":          {entry(`{"session":"s","prompt":"p","schedule":"@daily","timezone":"Mars/Base"}`), `timezone "Mars/Base" is not a known IANA time zone`},
		"zone without cron": {entry(`{"session":"s","prompt":"p","webhook":{"secret_env":"X"},"timezone":"UTC"}`), "timezone is for schedule"},
		"webhook no secret": {entry(`{"session":"s","prompt":"p","webhook":{}}`), "secret_env must name an environment variable"},
		"webhook bad env":   {entry(`{"session":"s","prompt":"p","webhook":{"secret_env":"1X"}}`), "secret_env must name an environment variable"},
		"webhook extra key": {entry(`{"session":"s","prompt":"p","webhook":{"secret_env":"X","secret":"y"}}`), "webhook.secret is not a setting"},
		"timeout negative":  {entry(`{"session":"s","prompt":"p","schedule":"@daily","timeout_ms":-1}`), "timeout_ms must be a safe integer between 0 and 86400000"},
		"prompt too long":   {entry(`{"session":"s","schedule":"@daily","prompt":"` + strings.Repeat("x", 65537) + `"}`), "prompt is longer than 65536 bytes"},
	} {
		_, err := normalizeServerSettings(json.RawMessage(`{"triggers":`+test.raw+`}`), "setting.json", "/base")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// The schema and the loader take and refuse the same trigger settings.
func TestTriggerSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "root-setting.schema.json#/properties/server/properties/triggers")
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{}`, true},
		{`{"enabled":true,"max_payload_bytes":1,"history_limit":10000,"signature_header":"X-Sig","entries":{}}`, true},
		{`{"entries":{"a.b-c_1":{"session":"s","prompt":"p","schedule":"@daily","timezone":"UTC","timeout_ms":0,"enabled":false}}}`, true},
		{`{"entries":{"ci":{"session":"s","prompt":"p","webhook":{"secret_env":"A_B"},"profile":"dev/x","model":"m"}}}`, true},
		{`{"entries":{"ci":{"session":"s","prompt":"p","schedule":"0 3 * * *","webhook":{"secret_env":"A"}}}}`, true},
		{`{"entries":{"ci":{"session":"s","prompt":"p"}}}`, false},
		{`{"entries":{"ci":{"prompt":"p","schedule":"@daily"}}}`, false},
		{`{"entries":{"ci":{"session":"s","schedule":"@daily"}}}`, false},
		{`{"entries":{"Ci":{"session":"s","prompt":"p","schedule":"@daily"}}}`, false},
		{`{"entries":{"ci":{"session":"s","prompt":"p","webhook":{}}}}`, false},
		{`{"entries":{"ci":{"session":"s","prompt":"p","webhook":{"secret_env":"9"}}}}`, false},
		{`{"entries":{"ci":{"session":"s","prompt":"p","schedule":"@daily","timeout_ms":86400001}}}`, false},
		{`{"max_payload_bytes":0}`, false},
		{`{"history_limit":10001}`, false},
		{`{"signature_header":"a b"}`, false},
		{`{"other":1}`, false},
		{`{"entries":{"ci":{"session":"s","prompt":"p","webhook":{"secret_env":"A"},"timezone":"UTC"}}}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := normalizeTriggers(json.RawMessage(test.raw), "s"); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}
