package anthropicadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// translation is the result of converting one Responses request.
type translation struct {
	Request anthropicRequest
	Stream  bool
	// Ignored names request fields the adapter dropped, for a response header.
	Ignored []string
}

// silentFields have no effect on the generated text, so dropping them is
// never reported.
var silentFields = map[string]bool{
	"store": true, "include": true, "user": true, "metadata": true,
	"prompt_cache_key": true, "prompt_cache_retention": true, "safety_identifier": true,
	"service_tier": true, "stream_options": true, "truncation": true,
}

// translateRequest converts a Responses API request body.
func translateRequest(config *Config, body []byte) (*translation, *apiError) {
	fields := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&fields); err != nil {
		return nil, badRequest("", "request body is not a JSON object: %v", err)
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, badRequest("", "invalid request: %v", err)
	}
	if request.Model == "" {
		return nil, badRequest("model", "model is required")
	}
	result := &translation{Stream: request.Stream}
	if apiErr := checkFields(config, fields, &request, result); apiErr != nil {
		return nil, apiErr
	}
	profile := config.ProfileFor(request.Model)

	out := &result.Request
	out.Model = request.Model
	out.MaxTokens = config.DefaultMaxTokens
	if request.MaxOutputTokens != nil {
		if *request.MaxOutputTokens < 1 {
			return nil, badRequest("max_output_tokens", "max_output_tokens must be at least 1")
		}
		out.MaxTokens = *request.MaxOutputTokens
	}
	out.Temperature = request.Temperature
	out.TopP = request.TopP
	out.Stream = request.Stream

	system, messages, apiErr := translateInput(config, profile, request.Instructions, request.Input)
	if apiErr != nil {
		return nil, apiErr
	}
	out.System, out.Messages = system, messages

	tools, apiErr := translateTools(request.Tools)
	if apiErr != nil {
		return nil, apiErr
	}
	out.Tools = tools
	choice, apiErr := translateToolChoice(request.ToolChoice, request.ParallelToolCalls, len(tools) > 0)
	if apiErr != nil {
		return nil, apiErr
	}
	out.ToolChoice = choice
	applyThinking(out, profile, request.Reasoning)
	sort.Strings(result.Ignored)
	return result, nil
}

// responsesRequest is the subset of a Responses request the adapter reads.
type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions"`
	Tools             []rawObject     `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Reasoning         *struct {
		Effort  string          `json:"effort"`
		Summary json.RawMessage `json:"summary"`
	} `json:"reasoning"`
}

type rawObject = map[string]json.RawMessage

// checkFields rejects what cannot be honoured without losing meaning, and
// applies the unsupported_fields policy to the rest.
func checkFields(config *Config, fields map[string]json.RawMessage, request *responsesRequest, result *translation) *apiError {
	handled := map[string]bool{
		"model": true, "input": true, "instructions": true, "tools": true,
		"tool_choice": true, "parallel_tool_calls": true, "max_output_tokens": true,
		"temperature": true, "top_p": true, "stream": true, "reasoning": true,
	}
	for name, raw := range fields {
		if handled[name] || silentFields[name] || isJSONNull(raw) {
			continue
		}
		switch name {
		case "previous_response_id", "conversation":
			return notImplemented("%s is not supported: the adapter keeps no conversation state, send the full input", name)
		case "background":
			if !isJSONFalse(raw) {
				return notImplemented("background responses are not supported")
			}
		case "text":
			if !textFormatIsPlain(raw) {
				if apiErr := applyPolicy(config, name, "text.format other than text", result); apiErr != nil {
					return apiErr
				}
			}
		default:
			if apiErr := applyPolicy(config, name, "this field", result); apiErr != nil {
				return apiErr
			}
		}
	}
	return nil
}

func applyPolicy(config *Config, name, what string, result *translation) *apiError {
	if config.UnsupportedFields == UnsupportedReject {
		return unsupported(name, "%s is not supported by the Anthropic adapter (%s)", name, what)
	}
	result.Ignored = append(result.Ignored, name)
	return nil
}

func isJSONNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func isJSONFalse(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "false" }

// textFormatIsPlain reports whether a Responses "text" option asks for plain
// text only. verbosity has no Anthropic equivalent and is dropped quietly.
func textFormatIsPlain(raw json.RawMessage) bool {
	var text struct {
		Format *struct {
			Type string `json:"type"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &text); err != nil {
		return false
	}
	return text.Format == nil || text.Format.Type == "" || text.Format.Type == "text"
}

// compactJSON returns raw with insignificant whitespace removed.
func compactJSON(raw []byte) json.RawMessage {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return json.RawMessage(raw)
	}
	return buffer.Bytes()
}

func describe(format string, args ...any) string { return fmt.Sprintf(format, args...) }
