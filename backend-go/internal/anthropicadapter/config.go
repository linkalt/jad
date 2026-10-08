package anthropicadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Config is the adapter's JSON configuration. Unknown keys are rejected so a
// misspelled key cannot silently fall back to a default.
type Config struct {
	SchemaVersion                int            `json:"schema_version"`
	Listen                       string         `json:"listen"`
	Upstream                     UpstreamConfig `json:"upstream"`
	DefaultMaxTokens             int            `json:"default_max_tokens"`
	MaxRequestBytes              int64          `json:"max_request_bytes"`
	UnsupportedFields            string         `json:"unsupported_fields"`
	SynthesizeMissingToolResults bool           `json:"synthesize_missing_tool_results"`
	MissingToolResultText        string         `json:"missing_tool_result_text"`
	InvalidArgumentsKey          string         `json:"invalid_arguments_key"`
	// StripSchemaKeywords are removed from the top level of every tool's input schema.
	// Anthropic answers 400 for oneOf, allOf and anyOf there.
	StripSchemaKeywords []string       `json:"strip_schema_keywords"`
	Log                 LogConfig      `json:"log"`
	Cache               CacheConfig    `json:"cache"`
	DefaultProfile      ModelProfile   `json:"default_profile"`
	Models              []ModelProfile `json:"models"`
}

// LogConfig controls what the adapter writes to stderr when an upstream call fails. Without it a
// failure reaches the caller as a short message and the response headers that explain it (the
// request id, Retry-After, the rate-limit headers) are lost. No request header value is logged,
// so the credential cannot reach the log.
type LogConfig struct {
	// UpstreamErrors turns the failure log on or off.
	UpstreamErrors bool `json:"upstream_errors"`
	// BodyBytes is how much of the response body (or error message) is logged. 0 logs none.
	BodyBytes int `json:"body_bytes"`
	// Headers are the response header names that are logged; an entry ending in * matches a prefix.
	Headers []string `json:"headers"`
}

// CacheConfig selects the prompt-caching breakpoints. Everything is off by
// default: a cache write costs more than a plain input token, so caching only
// pays when the same prefix is reused within the TTL.
type CacheConfig struct {
	// Automatic sends the top-level cache_control, which marks the last block
	// of the request.
	Automatic bool `json:"automatic"`
	// Tools marks the last tool definition.
	Tools bool `json:"tools"`
	// System marks the last system block. The cached prefix covers tools too.
	System bool `json:"system"`
	// History marks the last block that comes from a real conversation item,
	// skipping system or developer text appended after it. A caller that
	// appends changing context at the end of every request needs this: the
	// automatic breakpoint would land on that changing text and never hit.
	History bool `json:"history"`
	// TTL is "" for the 5-minute default or "1h".
	TTL string `json:"ttl"`
}

// UpstreamConfig describes the Anthropic endpoint.
type UpstreamConfig struct {
	BaseURL          string `json:"base_url"`
	AnthropicVersion string `json:"anthropic_version"`
	// AuthHeader is bearer or x-api-key: how the caller's token goes upstream.
	AuthHeader string   `json:"auth_header"`
	Beta       []string `json:"beta"`
	// CredentialFile, when set, is a file written by the claude-auth command. The adapter then
	// sends the access token stored in it, read on every request, and ignores the credential
	// the caller sent. Empty (the default) forwards the caller's credential.
	CredentialFile string `json:"credential_file"`
	// SystemPrefix, when not empty, is sent as the first system block of every request, before
	// the caller's instructions, as a block of its own. A Claude subscription token is refused
	// with 429 unless the request opens with the Claude Code sentence (found by a live probe).
	SystemPrefix string `json:"system_prefix"`
	// ExpiryMarginMS is how long before its stored expiry a credential-file token already
	// counts as expired. It applies only with credential_file.
	ExpiryMarginMS int `json:"expiry_margin_ms"`
	// ExtraHeaders are added to every upstream request, for a gateway or an endpoint that
	// wants more than the headers the adapter sets itself.
	ExtraHeaders     map[string]string `json:"extra_headers"`
	RequestTimeoutMS int               `json:"request_timeout_ms"`
	// StreamRetry retries a streaming request whose upstream answered with an
	// error event before any output was written.
	StreamRetry StreamRetryConfig `json:"stream_retry"`

	// credentialPath is CredentialFile with "~/" expanded.
	credentialPath string
}

// StreamRetryConfig controls the invisible retry of a stream that fails at
// its start. MaxRetries 0 turns it off.
type StreamRetryConfig struct {
	MaxRetries int      `json:"max_retries"`
	DelayMS    int      `json:"delay_ms"`
	ErrorTypes []string `json:"error_types"`
}

// ModelProfile holds the per-model rules. Anthropic models differ in which
// thinking parameters they accept, so these are data, not code.
type ModelProfile struct {
	// Match is a regular expression tested against the request's model name.
	// It is ignored in default_profile.
	Match string `json:"match,omitempty"`
	// Thinking is none, adaptive or budget.
	Thinking string `json:"thinking"`
	// Display is the thinking.display value to send, or empty to omit it.
	Display string `json:"display"`
	// DefaultEffort is used when the request names no reasoning.effort.
	DefaultEffort string `json:"default_effort"`
	// EffortMap maps a Responses effort to an Anthropic output_config.effort.
	EffortMap map[string]string `json:"effort_map"`
	// EffortBudget maps a Responses effort to budget_tokens in budget mode.
	EffortBudget map[string]int `json:"effort_budget"`
	// MidConversationSystem sends later system/developer messages as
	// role=system instead of folding them into a user message.
	MidConversationSystem bool `json:"mid_conversation_system"`

	pattern *regexp.Regexp
}

// DefaultConfig returns the configuration used when no file is given.
func DefaultConfig() Config {
	return Config{
		SchemaVersion: 1,
		Listen:        DefaultListen,
		Upstream: UpstreamConfig{
			BaseURL:          DefaultUpstreamBaseURL,
			AnthropicVersion: DefaultAnthropicVersion,
			AuthHeader:       DefaultAuthHeader,
			Beta:             []string{},
			CredentialFile:   DefaultCredentialFile,
			SystemPrefix:     DefaultSystemPrefix,
			ExpiryMarginMS:   DefaultExpiryMarginMS,
			ExtraHeaders:     map[string]string{},
			RequestTimeoutMS: DefaultRequestTimeoutMS,
			StreamRetry: StreamRetryConfig{
				MaxRetries: DefaultStreamRetryMax,
				DelayMS:    DefaultStreamRetryDelayMS,
				ErrorTypes: []string{"overloaded_error", "api_error"},
			},
		},
		DefaultMaxTokens:             DefaultMaxTokens,
		MaxRequestBytes:              DefaultMaxRequestBytes,
		UnsupportedFields:            DefaultUnsupportedFields,
		SynthesizeMissingToolResults: DefaultSynthesizeToolResults,
		MissingToolResultText:        DefaultMissingToolResultText,
		InvalidArgumentsKey:          DefaultInvalidArgumentsKey,
		StripSchemaKeywords:          defaultStripSchemaKeywords(),
		Log: LogConfig{
			UpstreamErrors: DefaultLogUpstreamErrors,
			BodyBytes:      DefaultLogBodyBytes,
			Headers:        defaultLogHeaders(),
		},
		DefaultProfile: defaultProfile(),
		Models:         []ModelProfile{},
	}
}

func defaultProfile() ModelProfile {
	return ModelProfile{
		Thinking:     ThinkingNone,
		Display:      DisplayDefault,
		EffortMap:    defaultEffortMap(),
		EffortBudget: defaultEffortBudget(),
	}
}

// LoadConfig reads and validates a configuration file. An empty path returns
// the defaults.
func LoadConfig(path string) (*Config, error) {
	config := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := decodeConfig(data, &config); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := config.finish(); err != nil {
		return nil, err
	}
	return &config, nil
}

func decodeConfig(data []byte, config *Config) error {
	// Each profile starts from the defaults so a partial profile keeps the
	// default effort tables; json merges into existing map entries.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if decoder.More() {
		return errors.New("parse config: trailing data after the JSON object")
	}
	return nil
}

// finish fills profile defaults, compiles patterns and validates every value.
func (c *Config) finish() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("config: schema_version must be 1, got %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("config: listen must be a non-empty host:port")
	}
	if err := c.Upstream.validate(); err != nil {
		return err
	}
	if c.DefaultMaxTokens < 1 {
		return fmt.Errorf("config: default_max_tokens must be at least 1, got %d", c.DefaultMaxTokens)
	}
	if c.MaxRequestBytes < 1 {
		return fmt.Errorf("config: max_request_bytes must be at least 1, got %d", c.MaxRequestBytes)
	}
	if c.UnsupportedFields != UnsupportedReject && c.UnsupportedFields != UnsupportedIgnore {
		return fmt.Errorf("config: unsupported_fields must be %q or %q, got %q",
			UnsupportedReject, UnsupportedIgnore, c.UnsupportedFields)
	}
	if strings.TrimSpace(c.InvalidArgumentsKey) == "" {
		return errors.New("config: invalid_arguments_key must be a non-empty string")
	}
	if strings.TrimSpace(c.MissingToolResultText) == "" {
		return errors.New("config: missing_tool_result_text must be a non-empty string")
	}
	if c.Log.BodyBytes < 0 || c.Log.BodyBytes > MaxLogBodyBytes {
		return fmt.Errorf("config: log.body_bytes must be 0..%d, got %d", MaxLogBodyBytes, c.Log.BodyBytes)
	}
	for _, name := range c.Log.Headers {
		if !validHeaderName(strings.TrimSuffix(name, "*")) {
			return fmt.Errorf("config: log.headers entry %q is not an HTTP header name (a trailing * makes it a prefix)", name)
		}
	}
	for _, keyword := range c.StripSchemaKeywords {
		if strings.TrimSpace(keyword) == "" {
			return errors.New("config: strip_schema_keywords entries must be non-empty strings")
		}
	}
	if c.Cache.TTL != CacheTTLDefault && c.Cache.TTL != CacheTTLOneHour {
		return fmt.Errorf("config: cache.ttl must be empty or %q, got %q", CacheTTLOneHour, c.Cache.TTL)
	}
	if err := c.DefaultProfile.finish("default_profile", false); err != nil {
		return err
	}
	for index := range c.Models {
		if err := c.Models[index].finish(fmt.Sprintf("models[%d]", index), true); err != nil {
			return err
		}
	}
	return nil
}

func (u *UpstreamConfig) validate() error {
	parsed, err := url.Parse(u.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("config: upstream.base_url must be an http or https URL, got %q", u.BaseURL)
	}
	if strings.TrimSpace(u.AnthropicVersion) == "" {
		return errors.New("config: upstream.anthropic_version must be a non-empty string")
	}
	if u.AuthHeader != AuthHeaderBearer && u.AuthHeader != AuthHeaderXAPIKey {
		return fmt.Errorf("config: upstream.auth_header must be %q or %q, got %q", AuthHeaderBearer, AuthHeaderXAPIKey, u.AuthHeader)
	}
	for _, beta := range u.Beta {
		if strings.TrimSpace(beta) == "" || strings.ContainsAny(beta, ", \t") {
			return fmt.Errorf("config: upstream.beta entries must be single non-empty tokens, got %q", beta)
		}
	}
	if u.CredentialFile != "" {
		path, err := ExpandCredentialPath(u.CredentialFile)
		if err != nil {
			return fmt.Errorf("config: upstream.credential_file: %w", err)
		}
		u.credentialPath = path
	}
	if u.ExpiryMarginMS < 0 || u.ExpiryMarginMS > MaxRequestTimeoutMS {
		return fmt.Errorf("config: upstream.expiry_margin_ms must be 0..%d, got %d", MaxRequestTimeoutMS, u.ExpiryMarginMS)
	}
	for name, value := range u.ExtraHeaders {
		if !validHeaderName(name) {
			return fmt.Errorf("config: upstream.extra_headers name %q is not a valid HTTP header name", name)
		}
		if slices.Contains(reservedUpstreamHeaders, strings.ToLower(name)) {
			return fmt.Errorf("config: upstream.extra_headers.%s is set by the adapter and cannot be overridden (use auth_header, beta or anthropic_version)", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("config: upstream.extra_headers.%s must not contain a line break", name)
		}
	}
	if u.RequestTimeoutMS < 1 || u.RequestTimeoutMS > MaxRequestTimeoutMS {
		return fmt.Errorf("config: upstream.request_timeout_ms must be 1..%d, got %d",
			MaxRequestTimeoutMS, u.RequestTimeoutMS)
	}
	return u.StreamRetry.validate()
}

func (r *StreamRetryConfig) validate() error {
	if r.MaxRetries < 0 || r.MaxRetries > MaxStreamRetryMax {
		return fmt.Errorf("config: upstream.stream_retry.max_retries must be 0..%d, got %d", MaxStreamRetryMax, r.MaxRetries)
	}
	if r.DelayMS < 0 || r.DelayMS > MaxStreamRetryDelayMS {
		return fmt.Errorf("config: upstream.stream_retry.delay_ms must be 0..%d, got %d", MaxStreamRetryDelayMS, r.DelayMS)
	}
	for _, kind := range r.ErrorTypes {
		if strings.TrimSpace(kind) == "" {
			return errors.New("config: upstream.stream_retry.error_types entries must be non-empty strings")
		}
	}
	return nil
}

func (p *ModelProfile) finish(path string, needsMatch bool) error {
	if p.Thinking == "" {
		p.Thinking = ThinkingNone
	}
	if !slices.Contains([]string{ThinkingNone, ThinkingAdaptive, ThinkingBudget}, p.Thinking) {
		return fmt.Errorf("config: %s.thinking must be none, adaptive or budget, got %q", path, p.Thinking)
	}
	if !slices.Contains([]string{DisplayDefault, DisplaySummarized, DisplayOmitted}, p.Display) {
		return fmt.Errorf("config: %s.display must be empty, summarized or omitted, got %q", path, p.Display)
	}
	if p.DefaultEffort != "" && !slices.Contains(efforts, p.DefaultEffort) {
		return fmt.Errorf("config: %s.default_effort must be one of %v, got %q", path, efforts, p.DefaultEffort)
	}
	p.EffortMap = withDefaults(p.EffortMap, defaultEffortMap())
	for effort, mapped := range p.EffortMap {
		if !slices.Contains(efforts, effort) {
			return fmt.Errorf("config: %s.effort_map key %q is not a reasoning effort", path, effort)
		}
		if !slices.Contains(anthropicEfforts, mapped) {
			return fmt.Errorf("config: %s.effort_map[%q] must be one of %v, got %q", path, effort, anthropicEfforts, mapped)
		}
	}
	p.EffortBudget = withDefaults(p.EffortBudget, defaultEffortBudget())
	for effort, budget := range p.EffortBudget {
		if !slices.Contains(efforts, effort) {
			return fmt.Errorf("config: %s.effort_budget key %q is not a reasoning effort", path, effort)
		}
		if budget < MinThinkingBudgetTokens {
			return fmt.Errorf("config: %s.effort_budget[%q] must be at least %d, got %d",
				path, effort, MinThinkingBudgetTokens, budget)
		}
	}
	if !needsMatch {
		return nil
	}
	if strings.TrimSpace(p.Match) == "" {
		return fmt.Errorf("config: %s.match must be a non-empty regular expression", path)
	}
	pattern, err := regexp.Compile(p.Match)
	if err != nil {
		return fmt.Errorf("config: %s.match is not a valid regular expression: %w", path, err)
	}
	p.pattern = pattern
	return nil
}

// withDefaults returns configured entries plus any default key not set.
func withDefaults[V any](configured, defaults map[string]V) map[string]V {
	merged := make(map[string]V, len(defaults)+len(configured))
	for key, value := range defaults {
		merged[key] = value
	}
	for key, value := range configured {
		merged[key] = value
	}
	return merged
}

// ProfileFor returns the first model profile whose pattern matches model, or
// the default profile.
func (c *Config) ProfileFor(model string) *ModelProfile {
	for index := range c.Models {
		if c.Models[index].pattern.MatchString(model) {
			return &c.Models[index]
		}
	}
	return &c.DefaultProfile
}
