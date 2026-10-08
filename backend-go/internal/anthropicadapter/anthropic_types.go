package anthropicadapter

import "encoding/json"

// anthropicRequest is the body of POST /v1/messages.
type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       []anthropicBlock       `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   *anthropicToolChoice   `json:"tool_choice,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
	TopP         *float64               `json:"top_p,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	// CacheControl at the top level turns on automatic prompt caching.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl marks a prompt-caching breakpoint.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one content block, request or response side. Only the
// fields of the block's Type are set.
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Thinking is a pointer so an omitted-display block keeps "thinking":"".
	Thinking  *string          `json:"thinking,omitempty"`
	Signature string           `json:"signature,omitempty"`
	Data      string           `json:"data,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`
	Title     string           `json:"title,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   []anthropicBlock `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	// CacheControl marks this block as a prompt-caching breakpoint.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	// folded is set on text that came from a system or developer message and
	// was placed among the messages. The history breakpoint skips it.
	folded bool
}

// anthropicSource is the source of an image or document block.
type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// anthropicResponse is a non-streaming POST /v1/messages result.
type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      anthropicUsage   `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
