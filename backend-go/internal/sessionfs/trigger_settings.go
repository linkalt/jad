package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
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
	// maxTriggerReplayWindowMS bounds server.triggers.replay_window_ms: one day.
	maxTriggerReplayWindowMS int64 = 24 * 60 * 60 * 1000
	// minTriggerTimestampWindowMS is the least replay_window_ms that timestamp_header takes:
	// one second, the timestamp's resolution.
	minTriggerTimestampWindowMS int64 = 1000
	// server.triggers.replay_max_records: the most accepted signatures the replay record
	// holds at once, and its upper bound.
	defaultTriggerReplayMaxRecords int64 = 10_000
	maxTriggerReplayMaxRecords     int64 = 1_000_000

	defaultTriggerNotifyTimeoutMS    int64 = 10_000
	maxTriggerNotifyTimeoutMS        int64 = 120_000
	defaultTriggerNotifyRetries      int64 = 2
	maxTriggerNotifyRetries          int64 = 10
	defaultTriggerNotifyRetryDelayMS int64 = 2_000
	maxTriggerNotifyRetryDelayMS     int64 = 600_000
	maxTriggerNotifyAnswerBytes      int64 = 65_536
	// maxTriggerNotifyTemplateBytes bounds a notifier's template.
	maxTriggerNotifyTemplateBytes = 16_384
	maxTriggerNotifiers           = 16
)

// Formats of a trigger notification body.
const (
	TriggerNotifyFormatJSON = "json"
	TriggerNotifyFormatText = "text"
	// TriggerNotifyFormatSlack posts {"text": ...} as a Slack incoming webhook takes it.
	TriggerNotifyFormatSlack = "slack"
)

// TriggerNotifyStatuses are the statuses a trigger run can end with, values of a notifier's
// on list.
var TriggerNotifyStatuses = []string{"completed", "failed", "timed_out", "aborted", "refused", "skipped"}

// TriggerNotifyStopHooksSkipped is the on value that reports a run whose stop commands were
// not run because it reached a cap, whatever its status: such a run ends completed, so a
// notifier that reports failures only would not hear of it.
const TriggerNotifyStopHooksSkipped = "stop_hooks_skipped"

// triggerNotifyOnValues are the values a notifier's on list takes.
var triggerNotifyOnValues = append(append([]string(nil), TriggerNotifyStatuses...), TriggerNotifyStopHooksSkipped)

// defaultTriggerNotifyOn leaves out skipped: a schedule that outruns its runs would send
// one every firing.
var defaultTriggerNotifyOn = []string{"completed", "failed", "timed_out", "aborted", "refused"}

var triggerNotifyKeys = []string{"url", "url_env", "format", "template", "headers", "headers_env", "on", "timeout_ms", "retries", "retry_delay_ms", "answer_bytes"}

// triggerHeaderPattern is an HTTP header name (an RFC 9110 token).
var triggerHeaderPattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

var triggerKeys = []string{"enabled", "max_payload_bytes", "history_limit", "signature_header", "replay_window_ms", "replay_max_records", "timestamp_header", "notify", "entries"}

var triggerEntryKeys = []string{"enabled", "session", "profile", "model", "prompt", "schedule", "timezone", "webhook", "timeout_ms", "notify"}

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
	// ReplayWindowMS, when above zero, refuses a webhook whose signature was already
	// accepted in that many milliseconds (409). Zero, the default, keeps no record.
	ReplayWindowMS int64
	// ReplayMaxRecords is the most signatures the replay record holds at once. With the
	// record full, a new signed request is refused (503) until records expire: dropping
	// one early would let its request run again.
	ReplayMaxRecords int64
	// TimestampHeader, when set, is the header that carries the request's time in Unix
	// seconds. The signature then covers "<timestamp>.<body>", and a request whose time is
	// more than ReplayWindowMS from the server's is refused (401). It needs
	// ReplayWindowMS. Empty, the default, signs the body alone.
	TimestampHeader string
	// Notifiers are server.triggers.notify, sorted by name.
	Notifiers []TriggerNotifier
	// Entries are sorted by name.
	Entries []TriggerEntry
}

// TriggerNotifier is one place a finished trigger run is reported to: an HTTP POST of the
// run's record (format json), of a short text (format text, which ntfy-style services
// show as the message) or of a Slack message (format slack). Template, when set, renders
// the body instead.
type TriggerNotifier struct {
	Name string
	// URL is the address; URLEnv instead names the environment variable that holds it, for
	// an address that is itself a secret. Exactly one is set.
	URL    string
	URLEnv string
	Format string
	// Template is a Go text/template the body is rendered from in place of the format's
	// own; the format then only sets the content type, and with json or slack the
	// rendered body must be JSON. Empty uses the format's body.
	Template string
	// Headers are sent as written; HeadersEnv maps a header to the environment variable
	// that holds its value (a token). Both are keyed by canonical header name.
	Headers    map[string]string
	HeadersEnv map[string]string
	// On lists the statuses that are reported, and TriggerNotifyStopHooksSkipped.
	On []string
	// TimeoutMS bounds one attempt; Retries more attempts follow a failed one, RetryDelayMS apart.
	TimeoutMS    int64
	Retries      int64
	RetryDelayMS int64
	// AnswerBytes is how much of the run's final answer is sent; 0 sends none.
	AnswerBytes int64
}

// Wants reports whether the notifier reports a run that ended with status; stopHooksSkipped
// is true when the run's stop commands were not run because it reached a cap.
func (n TriggerNotifier) Wants(status string, stopHooksSkipped bool) bool {
	return containsString(n.On, status) || (stopHooksSkipped && containsString(n.On, TriggerNotifyStopHooksSkipped))
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
	// Notify names the server.triggers.notify entries told when a run ends.
	Notify []string
}

func defaultTriggerSettings() TriggerSettings {
	return TriggerSettings{MaxPayloadBytes: defaultTriggerMaxPayloadBytes, HistoryLimit: defaultTriggerHistoryLimit, SignatureHeader: defaultTriggerSignatureHeader, ReplayMaxRecords: defaultTriggerReplayMaxRecords}
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
	if settings.ReplayWindowMS, err = configuredInteger(fields, "replay_window_ms", 0, 0, maxTriggerReplayWindowMS, path, section); err != nil {
		return TriggerSettings{}, err
	}
	if settings.ReplayMaxRecords, err = configuredInteger(fields, "replay_max_records", defaultTriggerReplayMaxRecords, 1, maxTriggerReplayMaxRecords, path, section); err != nil {
		return TriggerSettings{}, err
	}
	if settings.TimestampHeader, err = bashString(fields, "timestamp_header", "", path+": "+section, false); err != nil {
		return TriggerSettings{}, err
	}
	if settings.TimestampHeader != "" {
		if !triggerHeaderPattern.MatchString(settings.TimestampHeader) {
			return TriggerSettings{}, fmt.Errorf("%s: %s.timestamp_header must be an HTTP header name", path, section)
		}
		settings.TimestampHeader = textproto.CanonicalMIMEHeaderKey(settings.TimestampHeader)
		if settings.TimestampHeader == settings.SignatureHeader {
			return TriggerSettings{}, fmt.Errorf("%s: %s.timestamp_header must differ from signature_header", path, section)
		}
		if settings.ReplayWindowMS == 0 {
			return TriggerSettings{}, fmt.Errorf("%s: %s.timestamp_header needs replay_window_ms, the age a timestamp may have", path, section)
		}
		// The timestamp has whole-second resolution, so a shorter window would refuse a
		// request from a sender whose clock is exactly right.
		if settings.ReplayWindowMS < minTriggerTimestampWindowMS {
			return TriggerSettings{}, fmt.Errorf("%s: %s.timestamp_header needs replay_window_ms of at least %d (the timestamp counts whole seconds)", path, section, minTriggerTimestampWindowMS)
		}
	}
	if settings.Notifiers, err = normalizeTriggerNotifiers(fields["notify"], path); err != nil {
		return TriggerSettings{}, err
	}
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
		for _, notifier := range entry.Notify {
			if !slices.ContainsFunc(settings.Notifiers, func(known TriggerNotifier) bool { return known.Name == notifier }) {
				return TriggerSettings{}, fmt.Errorf("%s: %s.entries.%s.notify: %q is not in server.triggers.notify", path, section, name, notifier)
			}
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
	if rawNotify, exists := fields["notify"]; exists && string(rawNotify) != "null" {
		if err := json.Unmarshal(rawNotify, &entry.Notify); err != nil || entry.Notify == nil {
			return TriggerEntry{}, false, fmt.Errorf("%s.notify must be an array of server.triggers.notify names", prefix)
		}
		seen := map[string]bool{}
		for _, name := range entry.Notify {
			if seen[name] {
				return TriggerEntry{}, false, fmt.Errorf("%s.notify names %q twice", prefix, name)
			}
			seen[name] = true
		}
	}
	return entry, enabled, nil
}

func normalizeTriggerNotifiers(raw json.RawMessage, path string) ([]TriggerNotifier, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("%s: server.triggers.notify must be an object of named notifiers", path)
	}
	if len(entries) > maxTriggerNotifiers {
		return nil, fmt.Errorf("%s: server.triggers.notify has %d notifiers; at most %d", path, len(entries), maxTriggerNotifiers)
	}
	notifiers := make([]TriggerNotifier, 0, len(entries))
	for name, rawEntry := range entries {
		notifier, err := normalizeTriggerNotifier(name, rawEntry, path)
		if err != nil {
			return nil, err
		}
		notifiers = append(notifiers, notifier)
	}
	sort.Slice(notifiers, func(a, b int) bool { return notifiers[a].Name < notifiers[b].Name })
	return notifiers, nil
}

func normalizeTriggerNotifier(name string, raw json.RawMessage, path string) (TriggerNotifier, error) {
	section := "server.triggers.notify." + name
	prefix := path + ": " + section
	if !triggerNamePattern.MatchString(name) {
		return TriggerNotifier{}, fmt.Errorf("%s: server.triggers.notify: %q must be 1-64 of a-z, 0-9, _, . and -, starting with a letter or digit", path, name)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return TriggerNotifier{}, fmt.Errorf("%s must be an object", prefix)
	}
	for key := range fields {
		if !containsString(triggerNotifyKeys, key) {
			return TriggerNotifier{}, fmt.Errorf("%s.%s is not a setting (%s)", prefix, key, strings.Join(triggerNotifyKeys, ", "))
		}
	}
	notifier := TriggerNotifier{Name: name}
	var err error
	if notifier.URL, err = bashString(fields, "url", "", prefix, false); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.URLEnv, err = bashString(fields, "url_env", "", prefix, false); err != nil {
		return TriggerNotifier{}, err
	}
	switch {
	case (notifier.URL == "") == (notifier.URLEnv == ""):
		return TriggerNotifier{}, fmt.Errorf("%s needs exactly one of url and url_env", prefix)
	case notifier.URL != "":
		if err := CheckTriggerNotifyURL(notifier.URL); err != nil {
			return TriggerNotifier{}, fmt.Errorf("%s.url %v", prefix, err)
		}
	case !triggerEnvPattern.MatchString(notifier.URLEnv):
		return TriggerNotifier{}, fmt.Errorf("%s.url_env must name an environment variable", prefix)
	}
	if notifier.Format, err = bashString(fields, "format", TriggerNotifyFormatJSON, prefix, false); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.Format != TriggerNotifyFormatJSON && notifier.Format != TriggerNotifyFormatText && notifier.Format != TriggerNotifyFormatSlack {
		return TriggerNotifier{}, fmt.Errorf("%s.format must be json, text or slack", prefix)
	}
	if rawTemplate, exists := fields["template"]; exists && string(rawTemplate) != "null" {
		if json.Unmarshal(rawTemplate, &notifier.Template) != nil || strings.TrimSpace(notifier.Template) == "" || len(notifier.Template) > maxTriggerNotifyTemplateBytes {
			return TriggerNotifier{}, fmt.Errorf("%s.template must be a non-empty string of at most %d bytes", prefix, maxTriggerNotifyTemplateBytes)
		}
		if _, err := ParseTriggerNotifyTemplate(name, notifier.Template); err != nil {
			return TriggerNotifier{}, fmt.Errorf("%s.template: %v", prefix, err)
		}
	}
	if notifier.Headers, err = triggerNotifyHeaders(fields["headers"], prefix+".headers", false); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.HeadersEnv, err = triggerNotifyHeaders(fields["headers_env"], prefix+".headers_env", true); err != nil {
		return TriggerNotifier{}, err
	}
	for header := range notifier.HeadersEnv {
		if _, both := notifier.Headers[header]; both {
			return TriggerNotifier{}, fmt.Errorf("%s: %s is in both headers and headers_env", prefix, header)
		}
	}
	notifier.On = append([]string(nil), defaultTriggerNotifyOn...)
	if rawOn, exists := fields["on"]; exists && string(rawOn) != "null" {
		if err := json.Unmarshal(rawOn, &notifier.On); err != nil || len(notifier.On) == 0 {
			return TriggerNotifier{}, fmt.Errorf("%s.on must be a non-empty array of statuses (%s) and %s", prefix, strings.Join(TriggerNotifyStatuses, ", "), TriggerNotifyStopHooksSkipped)
		}
		seen := map[string]bool{}
		for _, status := range notifier.On {
			if !containsString(triggerNotifyOnValues, status) || seen[status] {
				return TriggerNotifier{}, fmt.Errorf("%s.on: %q is not a status or %s, or is listed twice (statuses: %s)", prefix, status, TriggerNotifyStopHooksSkipped, strings.Join(TriggerNotifyStatuses, ", "))
			}
			seen[status] = true
		}
	}
	if notifier.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultTriggerNotifyTimeoutMS, 1, maxTriggerNotifyTimeoutMS, path, section); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.Retries, err = configuredInteger(fields, "retries", defaultTriggerNotifyRetries, 0, maxTriggerNotifyRetries, path, section); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.RetryDelayMS, err = configuredInteger(fields, "retry_delay_ms", defaultTriggerNotifyRetryDelayMS, 0, maxTriggerNotifyRetryDelayMS, path, section); err != nil {
		return TriggerNotifier{}, err
	}
	if notifier.AnswerBytes, err = configuredInteger(fields, "answer_bytes", 0, 0, maxTriggerNotifyAnswerBytes, path, section); err != nil {
		return TriggerNotifier{}, err
	}
	return notifier, nil
}

// CheckTriggerNotifyURL checks a notifier address: absolute http or https with a host.
// The error never repeats the address, which may carry a secret.
func CheckTriggerNotifyURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

// triggerNotifyHeaders reads a headers object: header name to value, or, with fromEnv, to
// the environment variable that holds the value.
func triggerNotifyHeaders(raw json.RawMessage, section string, fromEnv bool) (map[string]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil || headers == nil {
		return nil, fmt.Errorf("%s must be an object of header names to strings", section)
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if !triggerHeaderPattern.MatchString(name) || canonical == "Host" || canonical == "Content-Length" {
			return nil, fmt.Errorf("%s: %q is not a header the notifier may set", section, name)
		}
		if _, twice := out[canonical]; twice {
			return nil, fmt.Errorf("%s names %s twice", section, canonical)
		}
		switch {
		case fromEnv && !triggerEnvPattern.MatchString(value):
			return nil, fmt.Errorf("%s.%s must name an environment variable", section, name)
		case !fromEnv && strings.ContainsAny(value, "\r\n"):
			return nil, fmt.Errorf("%s.%s must be one line", section, name)
		}
		out[canonical] = value
	}
	return out, nil
}
