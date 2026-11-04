// Package chatadapter serves the OpenAI Responses API on top of an OpenAI-compatible
// Chat Completions endpoint (POST {base_url}/chat/completions), for a model whose
// provider has no Responses API: vLLM, Ollama, llama.cpp, LM Studio, DeepSeek, Groq and
// most gateways. jad runs it in process (see internal/inprocess); it keeps no state, so
// every request carries the full history.
package chatadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Defaults and bounds of every configurable key. The model schema documents the same
// values; config_test.go keeps the two in step.
const (
	DefaultBaseURL               = "https://api.openai.com/v1"
	MaxTokensFieldCompletion     = "max_completion_tokens"
	MaxTokensFieldLegacy         = "max_tokens"
	DefaultMaxTokensField        = MaxTokensFieldCompletion
	DefaultIncludeUsage          = true
	DefaultSystemRole            = "system"
	DefaultEchoReasoningField    = ""
	DefaultMissingToolResultText = "No result was recorded for this tool call."
	DefaultRequestTimeoutMS      = 600_000
	MaxRequestTimeoutMS          = 3_600_000
	DefaultMaxEventBytes         = 16 << 20
	MaxMaxEventBytes             = 1 << 30
	DefaultMaxRequestBytes       = 64 << 20
	MaxMaxRequestBytes           = 1 << 30
)

// DefaultReasoningFields are the delta fields read as reasoning text, in this order:
// DeepSeek, vLLM and llama.cpp send reasoning_content; OpenRouter and Ollama send reasoning.
func DefaultReasoningFields() []string { return []string{"reasoning_content", "reasoning"} }

// reservedHeaders are set by the adapter; extra_headers cannot replace them.
var reservedHeaders = []string{"authorization", "content-type", "content-length", "host", "accept"}

// reservedBodyFields are built from the Responses request; extra_body cannot set them.
var reservedBodyFields = []string{"model", "messages", "tools", "tool_choice", "stream", "stream_options"}

// Config is the chat_completions object of a model file. Unknown keys are refused.
type Config struct {
	// BaseURL is the model file's base_url: the root /chat/completions is appended to.
	BaseURL string `json:"-"`
	// MaxTokensField is where max_output_tokens goes: max_completion_tokens (OpenAI's
	// current name) or max_tokens (what older servers read).
	MaxTokensField string `json:"max_tokens_field"`
	// IncludeUsage sends stream_options.include_usage, so the stream ends with the token
	// counts. A server that refuses stream_options needs false; usage is then zero.
	IncludeUsage bool `json:"include_usage"`
	// SystemRole is the role of the instructions and of system or developer messages:
	// system, or developer for OpenAI reasoning models that want it.
	SystemRole string `json:"system_role"`
	// ReasoningFields are the delta fields read as reasoning text. Empty reads none.
	ReasoningFields []string `json:"reasoning_fields"`
	// EchoReasoningField, when set, sends a reasoning item's text back on the next
	// assistant message under this field (DeepSeek's thinking mode with tool calls wants
	// reasoning_content back). Empty drops reasoning from the history.
	EchoReasoningField string `json:"echo_reasoning_field"`
	// MissingToolResultText answers a tool call the history has no result for: Chat
	// Completions refuses an assistant tool call that is not followed by its result.
	MissingToolResultText string `json:"missing_tool_result_text"`
	RequestTimeoutMS      int    `json:"request_timeout_ms"`
	// MaxEventBytes bounds one upstream SSE event; a larger one fails the response.
	MaxEventBytes int64 `json:"max_event_bytes"`
	// MaxRequestBytes bounds the Responses request the adapter accepts.
	MaxRequestBytes int64 `json:"max_request_bytes"`
	// ExtraHeaders are added to every upstream request.
	ExtraHeaders map[string]string `json:"extra_headers"`
	// ExtraBody holds top-level fields merged last into every request body, as raw JSON
	// (for example {"chat_template_kwargs":{"enable_thinking":true}} for vLLM).
	ExtraBody map[string]json.RawMessage `json:"extra_body"`
}

// DefaultConfig returns the configuration of a model file without a chat_completions object.
func DefaultConfig() Config {
	return Config{
		BaseURL:               DefaultBaseURL,
		MaxTokensField:        DefaultMaxTokensField,
		IncludeUsage:          DefaultIncludeUsage,
		SystemRole:            DefaultSystemRole,
		ReasoningFields:       DefaultReasoningFields(),
		EchoReasoningField:    DefaultEchoReasoningField,
		MissingToolResultText: DefaultMissingToolResultText,
		RequestTimeoutMS:      DefaultRequestTimeoutMS,
		MaxEventBytes:         DefaultMaxEventBytes,
		MaxRequestBytes:       DefaultMaxRequestBytes,
		ExtraHeaders:          map[string]string{},
		ExtraBody:             map[string]json.RawMessage{},
	}
}

// ParseConfig reads the chat_completions object of a model file; absent or null means
// the defaults. An empty baseURL keeps DefaultBaseURL.
func ParseConfig(raw json.RawMessage, baseURL string) (*Config, error) {
	config := DefaultConfig()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if trimmed[0] != '{' {
			return nil, errors.New("must be a JSON object")
		}
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		if decoder.More() {
			return nil, errors.New("parse: trailing data after the JSON object")
		}
	}
	if baseURL != "" {
		config.BaseURL = strings.TrimRight(baseURL, "/")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *Config) validate() error {
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("base_url must be an http or https URL, got %q", c.BaseURL)
	}
	if c.MaxTokensField != MaxTokensFieldCompletion && c.MaxTokensField != MaxTokensFieldLegacy {
		return fmt.Errorf("max_tokens_field must be %q or %q, got %q", MaxTokensFieldCompletion, MaxTokensFieldLegacy, c.MaxTokensField)
	}
	if c.SystemRole != "system" && c.SystemRole != "developer" {
		return fmt.Errorf("system_role must be system or developer, got %q", c.SystemRole)
	}
	for _, field := range c.ReasoningFields {
		if strings.TrimSpace(field) == "" {
			return errors.New("reasoning_fields entries must be non-empty strings")
		}
	}
	if c.EchoReasoningField != "" && (strings.TrimSpace(c.EchoReasoningField) != c.EchoReasoningField || slices.Contains([]string{"role", "content", "tool_calls", "tool_call_id", "name"}, c.EchoReasoningField)) {
		return fmt.Errorf("echo_reasoning_field %q is not usable as a message field", c.EchoReasoningField)
	}
	if strings.TrimSpace(c.MissingToolResultText) == "" {
		return errors.New("missing_tool_result_text must be a non-empty string")
	}
	if c.RequestTimeoutMS < 1 || c.RequestTimeoutMS > MaxRequestTimeoutMS {
		return fmt.Errorf("request_timeout_ms must be 1..%d, got %d", MaxRequestTimeoutMS, c.RequestTimeoutMS)
	}
	if c.MaxEventBytes < 1 || c.MaxEventBytes > MaxMaxEventBytes {
		return fmt.Errorf("max_event_bytes must be 1..%d, got %d", MaxMaxEventBytes, c.MaxEventBytes)
	}
	if c.MaxRequestBytes < 1 || c.MaxRequestBytes > MaxMaxRequestBytes {
		return fmt.Errorf("max_request_bytes must be 1..%d, got %d", MaxMaxRequestBytes, c.MaxRequestBytes)
	}
	for name, value := range c.ExtraHeaders {
		if !validHeaderName(name) {
			return fmt.Errorf("extra_headers: %q is not an HTTP header name", name)
		}
		if slices.Contains(reservedHeaders, strings.ToLower(name)) {
			return fmt.Errorf("extra_headers: %s is set by the adapter", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("extra_headers: the value of %s has a line break", name)
		}
	}
	for name, value := range c.ExtraBody {
		if slices.Contains(reservedBodyFields, name) {
			return fmt.Errorf("extra_body: %s is built from the request", name)
		}
		if !json.Valid(value) {
			return fmt.Errorf("extra_body: %s is not valid JSON", name)
		}
	}
	return nil
}

// validHeaderName reports whether name is an RFC 9110 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}
