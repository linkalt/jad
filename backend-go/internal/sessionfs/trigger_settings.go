package sessionfs

import (
	"encoding/json"
	"fmt"
	"net/textproto"
	"regexp"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/cronexpr"
)

const (
	defaultTriggerMaxPayloadBytes int64 = 65536
	maxTriggerMaxPayloadBytes     int64 = 1 << 20
	defaultTriggerHistoryLimit    int64 = 100
	maxTriggerHistoryLimit        int64 = 10000
	maxTriggerTimeoutMS           int64 = 24 * 60 * 60 * 1000
	maxTriggerEntries                   = 64
	maxTriggerPromptBytes               = 65536
	defaultTriggerSignatureHeader       = "X-Hub-Signature-256"
	defaultTriggerTimezone              = "Local"
)

var triggerKeys = []string{"enabled", "max_payload_bytes", "history_limit", "signature_header", "entries"}

var triggerEntryKeys = []string{"enabled", "session", "profile", "model", "prompt", "schedule", "timezone", "webhook", "timeout_ms"}

var triggerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

var triggerEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TriggerSettings is server.triggers: runs the server starts by itself, on a cron schedule
// or when a signed webhook arrives. Off by default.
type TriggerSettings struct {
	Enabled bool
	// MaxPayloadBytes is the largest webhook body accepted; a larger one is refused (413).
	MaxPayloadBytes int64
	// HistoryLimit is how many finished trigger runs GET /api/triggers keeps.
	HistoryLimit int64
	// SignatureHeader carries the HMAC-SHA256 of the body, hex, with or without "sha256=".
	SignatureHeader string
	// Entries are sorted by name.
	Entries []TriggerEntry
}

// TriggerEntry is one trigger: the run it starts and what starts it.
type TriggerEntry struct {
	Name    string
	Session string
	Profile string
	Model   string
	// Prompt is the user message; {trigger}, {time} and {payload} (the webhook body) are filled in.
	Prompt string
	// Schedule is a cron expression, empty for a webhook-only trigger; Location is its zone.
	Schedule string
	Cron     *cronexpr.Schedule `json:"-"`
	Timezone string
	Location *time.Location `json:"-"`
	// WebhookSecretEnv names the environment variable with the HMAC secret; empty means the
	// trigger has no webhook.
	WebhookSecretEnv string
	// TimeoutMS stops a run that has not ended; zero leaves only the run's own limits.
	TimeoutMS int64
}

func defaultTriggerSettings() TriggerSettings {
	return TriggerSettings{MaxPayloadBytes: defaultTriggerMaxPayloadBytes, HistoryLimit: defaultTriggerHistoryLimit, SignatureHeader: defaultTriggerSignatureHeader}
}

func normalizeTriggers(raw json.RawMessage, path string) (TriggerSettings, error) {
	settings := defaultTriggerSettings()
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return TriggerSettings{}, fmt.Errorf("%s: server.triggers must be an object", path)
	}
	for key := range fields {
		if !containsString(triggerKeys, key) {
			return TriggerSettings{}, fmt.Errorf("%s: server.triggers.%s is not a setting (%s)", path, key, strings.Join(triggerKeys, ", "))
		}
	}
	section := "server.triggers"
	var err error
	if settings.Enabled, err = bashBool(fields, "enabled", false, path+": "+section); err != nil {
		return TriggerSettings{}, err
	}
	if settings.MaxPayloadBytes, err = configuredInteger(fields, "max_payload_bytes", defaultTriggerMaxPayloadBytes, 1, maxTriggerMaxPayloadBytes, path, section); err != nil {
		return TriggerSettings{}, err
	}
	if settings.HistoryLimit, err = configuredInteger(fields, "history_limit", defaultTriggerHistoryLimit, 1, maxTriggerHistoryLimit, path, section); err != nil {
		return TriggerSettings{}, err
	}
	if settings.SignatureHeader, err = bashString(fields, "signature_header", defaultTriggerSignatureHeader, path+": "+section, false); err != nil {
		return TriggerSettings{}, err
	}
	if strings.ContainsAny(settings.SignatureHeader, " :\t") {
		return TriggerSettings{}, fmt.Errorf("%s: %s.signature_header must be an HTTP header name", path, section)
	}
	settings.SignatureHeader = textproto.CanonicalMIMEHeaderKey(settings.SignatureHeader)
	rawEntries, exists := fields["entries"]
	if !exists || string(rawEntries) == "null" {
		return settings, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(rawEntries, &entries); err != nil || entries == nil {
		return TriggerSettings{}, fmt.Errorf("%s: %s.entries must be an object of named triggers", path, section)
	}
	if len(entries) > maxTriggerEntries {
		return TriggerSettings{}, fmt.Errorf("%s: %s.entries has %d triggers; at most %d", path, section, len(entries), maxTriggerEntries)
	}
	for name, rawEntry := range entries {
		entry, enabled, err := normalizeTriggerEntry(name, rawEntry, path)
		if err != nil {
			return TriggerSettings{}, err
		}
		if enabled {
			settings.Entries = append(settings.Entries, entry)
		}
	}
	sort.Slice(settings.Entries, func(a, b int) bool { return settings.Entries[a].Name < settings.Entries[b].Name })
	return settings, nil
}

func normalizeTriggerEntry(name string, raw json.RawMessage, path string) (TriggerEntry, bool, error) {
	section := "server.triggers.entries." + name
	if !triggerNamePattern.MatchString(name) {
		return TriggerEntry{}, false, fmt.Errorf("%s: server.triggers.entries: %q must be 1-64 of a-z, 0-9, _, . and -, starting with a letter or digit", path, name)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return TriggerEntry{}, false, fmt.Errorf("%s: %s must be an object", path, section)
	}
	for key := range fields {
		if !containsString(triggerEntryKeys, key) {
			return TriggerEntry{}, false, fmt.Errorf("%s: %s.%s is not a setting (%s)", path, section, key, strings.Join(triggerEntryKeys, ", "))
		}
	}
	prefix := path + ": " + section
	enabled, err := bashBool(fields, "enabled", true, prefix)
	if err != nil {
		return TriggerEntry{}, false, err
	}
	entry := TriggerEntry{Name: name}
	if entry.Session, err = bashString(fields, "session", "", prefix, false); err != nil || entry.Session == "" {
		return TriggerEntry{}, false, fmt.Errorf("%s.session must name a session (for example demo or root:demo)", prefix)
	}
	if entry.Prompt, err = bashString(fields, "prompt", "", prefix, false); err != nil || entry.Prompt == "" {
		return TriggerEntry{}, false, fmt.Errorf("%s.prompt must be a non-empty string", prefix)
	}
	if len(entry.Prompt) > maxTriggerPromptBytes {
		return TriggerEntry{}, false, fmt.Errorf("%s.prompt is longer than %d bytes", prefix, maxTriggerPromptBytes)
	}
	for _, field := range []struct {
		name   string
		target *string
	}{{"profile", &entry.Profile}, {"model", &entry.Model}, {"schedule", &entry.Schedule}} {
		if *field.target, err = bashString(fields, field.name, "", prefix, false); err != nil {
			return TriggerEntry{}, false, err
		}
	}
	if entry.Schedule != "" {
		if entry.Cron, err = cronexpr.Parse(entry.Schedule); err != nil {
			return TriggerEntry{}, false, fmt.Errorf("%s.schedule: %v", prefix, err)
		}
	}
	if entry.Timezone, err = bashString(fields, "timezone", defaultTriggerTimezone, prefix, false); err != nil {
		return TriggerEntry{}, false, err
	}
	if _, set := fields["timezone"]; set && entry.Schedule == "" {
		return TriggerEntry{}, false, fmt.Errorf("%s.timezone is for schedule, which is not set", prefix)
	}
	if entry.Location, err = time.LoadLocation(entry.Timezone); err != nil {
		return TriggerEntry{}, false, fmt.Errorf("%s.timezone %q is not a known IANA time zone (is tzdata installed?): %v", prefix, entry.Timezone, err)
	}
	if rawWebhook, exists := fields["webhook"]; exists && string(rawWebhook) != "null" {
		var webhook map[string]json.RawMessage
		if err := json.Unmarshal(rawWebhook, &webhook); err != nil || webhook == nil {
			return TriggerEntry{}, false, fmt.Errorf("%s.webhook must be an object with secret_env", prefix)
		}
		for key := range webhook {
			if key != "secret_env" {
				return TriggerEntry{}, false, fmt.Errorf("%s.webhook.%s is not a setting (secret_env)", prefix, key)
			}
		}
		if entry.WebhookSecretEnv, err = bashString(webhook, "secret_env", "", prefix+".webhook", false); err != nil || !triggerEnvPattern.MatchString(entry.WebhookSecretEnv) {
			return TriggerEntry{}, false, fmt.Errorf("%s.webhook.secret_env must name an environment variable", prefix)
		}
	}
	if entry.Schedule == "" && entry.WebhookSecretEnv == "" {
		return TriggerEntry{}, false, fmt.Errorf("%s needs a schedule, a webhook or both", prefix)
	}
	if entry.TimeoutMS, err = configuredInteger(fields, "timeout_ms", 0, 0, maxTriggerTimeoutMS, path, section); err != nil {
		return TriggerEntry{}, false, err
	}
	return entry, enabled, nil
}
