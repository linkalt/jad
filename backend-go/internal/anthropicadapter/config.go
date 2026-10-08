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
	DefaultProfile               ModelProfile   `json:"default_profile"`
	Models                       []ModelProfile `json:"models"`
}

// UpstreamConfig describes the Anthropic endpoint.
type UpstreamConfig struct {
	BaseURL          string   `json:"base_url"`
	AnthropicVersion string   `json:"anthropic_version"`
	Beta             []string `json:"beta"`
	RequestTimeoutMS int      `json:"request_timeout_ms"`
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
			Beta:             []string{},
			RequestTimeoutMS: DefaultRequestTimeoutMS,
		},
		DefaultMaxTokens:             DefaultMaxTokens,
		MaxRequestBytes:              DefaultMaxRequestBytes,
		UnsupportedFields:            DefaultUnsupportedFields,
		SynthesizeMissingToolResults: DefaultSynthesizeToolResults,
		MissingToolResultText:        DefaultMissingToolResultText,
		InvalidArgumentsKey:          DefaultInvalidArgumentsKey,
		DefaultProfile:               defaultProfile(),
		Models:                       []ModelProfile{},
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
	for _, beta := range u.Beta {
		if strings.TrimSpace(beta) == "" || strings.ContainsAny(beta, ", \t") {
			return fmt.Errorf("config: upstream.beta entries must be single non-empty tokens, got %q", beta)
		}
	}
	if u.RequestTimeoutMS < 1 || u.RequestTimeoutMS > MaxRequestTimeoutMS {
		return fmt.Errorf("config: upstream.request_timeout_ms must be 1..%d, got %d",
			MaxRequestTimeoutMS, u.RequestTimeoutMS)
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
