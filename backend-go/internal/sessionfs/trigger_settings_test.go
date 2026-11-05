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
		{`{"replay_window_ms":0}`, true},
		{`{"replay_window_ms":86400000,"timestamp_header":"X-Time"}`, true},
		{`{"replay_window_ms":-1}`, false},
		{`{"replay_window_ms":86400001}`, false},
		{`{"timestamp_header":"X-Time"}`, false},
		{`{"replay_window_ms":1,"timestamp_header":"X Time"}`, false},
		{`{"replay_window_ms":1,"timestamp_header":""}`, false},
		{`{"replay_window_ms":999,"timestamp_header":"X-Time"}`, false},
		{`{"replay_window_ms":1000,"timestamp_header":"X-Time"}`, true},
		{`{"replay_window_ms":999}`, true},
		{`{"replay_max_records":1000000}`, true},
		{`{"replay_max_records":0}`, false},
		{`{"replay_max_records":1000001}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := normalizeTriggers(json.RawMessage(test.raw), "s"); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}

func TestTriggerNotifySettings(t *testing.T) {
	settings, err := normalizeTriggers(json.RawMessage(`{"notify":{
		"phone":{"url":"https://ntfy.example/jad","format":"text","headers":{"title":"jad"},"on":["failed","skipped"],"answer_bytes":200},
		"ops":{"url_env":"OPS_HOOK","headers_env":{"authorization":"OPS_TOKEN"},"timeout_ms":500,"retries":0,"retry_delay_ms":0}},
		"entries":{"ci":{"session":"s","prompt":"p","schedule":"@daily","notify":["phone","ops"]}}}`), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Notifiers) != 2 || settings.Notifiers[0].Name != "ops" || settings.Notifiers[1].Name != "phone" {
		t.Fatalf("notifiers = %+v", settings.Notifiers)
	}
	ops, phone := settings.Notifiers[0], settings.Notifiers[1]
	if ops.URLEnv != "OPS_HOOK" || ops.URL != "" || ops.Format != TriggerNotifyFormatJSON || ops.HeadersEnv["Authorization"] != "OPS_TOKEN" || ops.TimeoutMS != 500 || ops.Retries != 0 || ops.RetryDelayMS != 0 || ops.AnswerBytes != 0 ||
		strings.Join(ops.On, ",") != "completed,failed,timed_out,aborted,refused" || ops.Wants("skipped") || !ops.Wants("completed") {
		t.Fatalf("ops = %+v", ops)
	}
	if phone.URL != "https://ntfy.example/jad" || phone.Format != TriggerNotifyFormatText || phone.Headers["Title"] != "jad" || phone.AnswerBytes != 200 || phone.TimeoutMS != 10000 || phone.Retries != 2 || phone.RetryDelayMS != 2000 ||
		!phone.Wants("skipped") || phone.Wants("completed") {
		t.Fatalf("phone = %+v", phone)
	}
	if notify := settings.Entries[0].Notify; strings.Join(notify, ",") != "phone,ops" {
		t.Fatalf("entry notify = %v", notify)
	}

	notifier := func(body string) string { return `{"notify":{"n":` + body + `}}` }
	for name, test := range map[string]struct{ raw, want string }{
		"not an object":       {`{"notify":[]}`, "notify must be an object of named notifiers"},
		"bad name":            {`{"notify":{"N":{"url":"https://x"}}}`, `"N" must be 1-64`},
		"unknown key":         {notifier(`{"url":"https://x","method":"PUT"}`), "notify.n.method is not a setting"},
		"no address":          {notifier(`{}`), "needs exactly one of url and url_env"},
		"both addresses":      {notifier(`{"url":"https://x","url_env":"X"}`), "needs exactly one of url and url_env"},
		"not http":            {notifier(`{"url":"ftp://x/secret"}`), "must be an absolute http or https URL"},
		"relative":            {notifier(`{"url":"/hook"}`), "must be an absolute http or https URL"},
		"bad env":             {notifier(`{"url_env":"1X"}`), "url_env must name an environment variable"},
		"bad format":          {notifier(`{"url":"https://x","format":"xml"}`), "format must be json, text or slack"},
		"host header":         {notifier(`{"url":"https://x","headers":{"host":"y"}}`), `"host" is not a header`},
		"length header":       {notifier(`{"url":"https://x","headers":{"Content-Length":"1"}}`), "is not a header"},
		"header with space":   {notifier(`{"url":"https://x","headers":{"X A":"1"}}`), "is not a header"},
		"header twice":        {notifier(`{"url":"https://x","headers":{"x-a":"1","X-A":"2"}}`), "names X-A twice"},
		"header two lines":    {notifier(`{"url":"https://x","headers":{"X-A":"1\r\nX-B: 2"}}`), "must be one line"},
		"header env bad":      {notifier(`{"url":"https://x","headers_env":{"X-A":"not an env"}}`), "must name an environment variable"},
		"header in both":      {notifier(`{"url":"https://x","headers":{"X-A":"1"},"headers_env":{"x-a":"A"}}`), "X-A is in both headers and headers_env"},
		"on empty":            {notifier(`{"url":"https://x","on":[]}`), "on must be a non-empty array"},
		"on unknown":          {notifier(`{"url":"https://x","on":["done"]}`), `"done" is not a status`},
		"on twice":            {notifier(`{"url":"https://x","on":["failed","failed"]}`), "listed twice"},
		"timeout zero":        {notifier(`{"url":"https://x","timeout_ms":0}`), "timeout_ms must be a safe integer between 1 and 120000"},
		"retries too many":    {notifier(`{"url":"https://x","retries":11}`), "retries must be a safe integer between 0 and 10"},
		"answer too large":    {notifier(`{"url":"https://x","answer_bytes":65537}`), "answer_bytes must be a safe integer between 0 and 65536"},
		"empty template":      {notifier(`{"url":"https://x","template":" "}`), "template must be a non-empty string"},
		"template not parsed": {notifier(`{"url":"https://x","template":"{{.Text"}`), "template: template: n:1: unclosed action"},
		"unknown function":    {notifier(`{"url":"https://x","template":"{{shout .Text}}"}`), `function "shout" not defined`},
		"entry unknown name":  {`{"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":["ops"]}}}`, `notify: "ops" is not in server.triggers.notify`},
		"entry notify twice":  {`{"notify":{"ops":{"url":"https://x"}},"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":["ops","ops"]}}}`, `notify names "ops" twice`},
		"entry notify string": {`{"notify":{"ops":{"url":"https://x"}},"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":"ops"}}}`, "notify must be an array"},
	} {
		_, err := normalizeTriggers(json.RawMessage(test.raw), "s")
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error repeats the address: %v", name, err)
		}
	}
}

// The schema and the loader take and refuse the same notifier settings, except what the
// schema cannot say: a notify name missing from server.triggers.notify, Host and
// Content-Length, and one header written in two cases.
func TestTriggerNotifySchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "root-setting.schema.json#/properties/server/properties/triggers")
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{"notify":{}}`, true},
		{`{"notify":{"a":{"url":"https://ntfy.sh/t","format":"text","headers":{"Title":"x"},"on":["skipped"],"timeout_ms":1,"retries":10,"retry_delay_ms":0,"answer_bytes":65536}}}`, true},
		{`{"notify":{"a":{"url_env":"HOOK","headers_env":{"Authorization":"TOKEN"}}},"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":["a"]}}}`, true},
		{`{"notify":{"a":{}}}`, false},
		{`{"notify":{"a":{"url":"https://x","url_env":"X"}}}`, false},
		{`{"notify":{"a":{"url":"ftp://x"}}}`, false},
		{`{"notify":{"a":{"url_env":"9X"}}}`, false},
		{`{"notify":{"a":{"url":"https://x","format":"xml"}}}`, false},
		{`{"notify":{"a":{"url":"https://x","format":"slack","template":"{\"text\": {{json .Text}}}"}}}`, true},
		{`{"notify":{"a":{"url":"https://x","template":""}}}`, false},
		{`{"notify":{"a":{"url":"https://x","template":7}}}`, false},
		{`{"notify":{"a":{"url":"https://x","headers":{"X A":"1"}}}}`, false},
		{`{"notify":{"a":{"url":"https://x","headers":{"X-A":"1\nX-B: 2"}}}}`, false},
		{`{"notify":{"a":{"url":"https://x","headers_env":{"X-A":"a b"}}}}`, false},
		{`{"notify":{"a":{"url":"https://x","on":[]}}}`, false},
		{`{"notify":{"a":{"url":"https://x","on":["done"]}}}`, false},
		{`{"notify":{"a":{"url":"https://x","on":["failed","failed"]}}}`, false},
		{`{"notify":{"a":{"url":"https://x","timeout_ms":120001}}}`, false},
		{`{"notify":{"a":{"url":"https://x","retries":-1}}}`, false},
		{`{"notify":{"a":{"url":"https://x","answer_bytes":65537}}}`, false},
		{`{"notify":{"a":{"url":"https://x","extra":1}}}`, false},
		{`{"notify":{"A":{"url":"https://x"}}}`, false},
		{`{"notify":{"a":{"url":"https://x"}},"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":["a","a"]}}}`, false},
		{`{"notify":{"a":{"url":"https://x"}},"entries":{"t":{"session":"s","prompt":"p","schedule":"@daily","notify":"a"}}}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := normalizeTriggers(json.RawMessage(test.raw), "s"); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}

func TestTriggerReplaySettings(t *testing.T) {
	got, err := normalizeTriggers(json.RawMessage(`{}`), "s")
	if err != nil || got.ReplayWindowMS != 0 || got.TimestampHeader != "" || got.ReplayMaxRecords != 10_000 {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	if got, err = normalizeTriggers(json.RawMessage(`{"replay_max_records":5}`), "s"); err != nil || got.ReplayMaxRecords != 5 {
		t.Fatalf("replay_max_records = %+v, %v", got, err)
	}
	got, err = normalizeTriggers(json.RawMessage(`{"replay_window_ms":300000,"timestamp_header":"x-jad-timestamp"}`), "s")
	if err != nil || got.ReplayWindowMS != 300000 || got.TimestampHeader != "X-Jad-Timestamp" {
		t.Fatalf("values = %+v, %v", got, err)
	}
	for raw, want := range map[string]string{
		`{"replay_window_ms":-1}`:                                            "replay_window_ms must be a safe integer between 0 and 86400000",
		`{"replay_window_ms":86400001}`:                                      "replay_window_ms must be a safe integer between 0 and 86400000",
		`{"replay_max_records":0}`:                                           "replay_max_records must be a safe integer between 1 and 1000000",
		`{"timestamp_header":"X-Time"}`:                                      "timestamp_header needs replay_window_ms",
		`{"replay_window_ms":999,"timestamp_header":"X-Time"}`:               "timestamp_header needs replay_window_ms of at least 1000",
		`{"replay_window_ms":1000,"timestamp_header":"X Time"}`:              "timestamp_header must be an HTTP header name",
		`{"replay_window_ms":1000,"timestamp_header":""}`:                    "timestamp_header must be a non-empty string",
		`{"replay_window_ms":1000,"timestamp_header":"x-hub-signature-256"}`: "timestamp_header must differ from signature_header",
	} {
		if _, err := normalizeTriggers(json.RawMessage(raw), "s"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", raw, err, want)
		}
	}
}
