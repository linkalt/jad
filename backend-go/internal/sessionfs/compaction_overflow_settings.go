package sessionfs

import (
	"encoding/json"
	"strings"
)

// CompactionOverflow is compaction.overflow: when the provider refuses a request because
// the history is larger than the model's context window, the run compacts and sends the
// round again, up to MaxAttempts times. It is off by default and needs
// compaction.auto_enabled.
type CompactionOverflow struct {
	Enabled     bool
	MaxAttempts int64
	// Patterns are matched, case-insensitively, against the provider's error: the code and
	// message of a failed stream, or the text of any other error.
	Patterns []string
	// ToolOutputMaxBytes is what an overflow does when only the run's own turn is left to
	// compact: the round is sent again with each tool output cut to about this many bytes
	// (each further attempt halves it, down to MinCompactionOverflowToolOutputMaxBytes). 0
	// turns it off; the transcript keeps the outputs whole either way.
	ToolOutputMaxBytes int64
}

const (
	defaultCompactionOverflowEnabled     = false
	defaultCompactionOverflowMaxAttempts = 1
	maxCompactionOverflowMaxAttempts     = 5
	maxCompactionOverflowPatterns        = 64
	// defaultCompactionOverflowToolOutputMaxBytes is below the usual output caps of the
	// native tools, so the first cut shrinks their outputs.
	defaultCompactionOverflowToolOutputMaxBytes = 8192
	maxCompactionOverflowToolOutputMaxBytes     = 1_000_000
	// MinCompactionOverflowToolOutputMaxBytes is the smallest tool_output_max_bytes other than
	// 0, and the floor later attempts halve the cut to.
	MinCompactionOverflowToolOutputMaxBytes = 256
)

var (
	compactionOverflowKeys = []string{"enabled", "max_attempts", "patterns", "tool_output_max_bytes"}
	// defaultCompactionOverflowPatterns are the context-window errors of the providers jad
	// speaks to: OpenAI (code context_length_exceeded), Anthropic ("prompt is too long"),
	// and the wordings of common OpenAI-compatible servers.
	defaultCompactionOverflowPatterns = []string{
		"context_length_exceeded",
		"context window",
		"maximum context length",
		"prompt is too long",
		"input is too long",
		"too many tokens",
		"exceeds the context",
		"context length",
	}
)

// DefaultCompactionOverflow returns compaction.overflow with every default.
func DefaultCompactionOverflow() CompactionOverflow {
	return CompactionOverflow{
		Enabled:            defaultCompactionOverflowEnabled,
		MaxAttempts:        defaultCompactionOverflowMaxAttempts,
		Patterns:           append([]string(nil), defaultCompactionOverflowPatterns...),
		ToolOutputMaxBytes: defaultCompactionOverflowToolOutputMaxBytes,
	}
}

func normalizeCompactionOverflow(raw json.RawMessage, path string) (CompactionOverflow, error) {
	settings := DefaultCompactionOverflow()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return CompactionOverflow{}, configError("%s: compaction.overflow must be an object", path)
	}
	if err := rejectUnknownKeys(fields, path+": compaction.overflow", compactionOverflowKeys); err != nil {
		return CompactionOverflow{}, err
	}
	prefix := path + ": compaction.overflow"
	var err error
	if settings.Enabled, err = bashBool(fields, "enabled", settings.Enabled, prefix); err != nil {
		return CompactionOverflow{}, err
	}
	if settings.MaxAttempts, err = configuredInteger(fields, "max_attempts", defaultCompactionOverflowMaxAttempts, 1, maxCompactionOverflowMaxAttempts, path, "compaction.overflow"); err != nil {
		return CompactionOverflow{}, err
	}
	if value, exists := fields["patterns"]; exists && strings.TrimSpace(string(value)) != "null" {
		var patterns []string
		if err := json.Unmarshal(value, &patterns); err != nil || len(patterns) == 0 || len(patterns) > maxCompactionOverflowPatterns {
			return CompactionOverflow{}, configError("%s.patterns must be a list of 1 to %d non-empty strings", prefix, maxCompactionOverflowPatterns)
		}
		for _, pattern := range patterns {
			if strings.TrimSpace(pattern) == "" {
				return CompactionOverflow{}, configError("%s.patterns must be a list of 1 to %d non-empty strings", prefix, maxCompactionOverflowPatterns)
			}
		}
		settings.Patterns = patterns
	}
	if settings.ToolOutputMaxBytes, err = configuredInteger(fields, "tool_output_max_bytes", defaultCompactionOverflowToolOutputMaxBytes, 0, maxCompactionOverflowToolOutputMaxBytes, path, "compaction.overflow"); err != nil {
		return CompactionOverflow{}, err
	}
	if settings.ToolOutputMaxBytes > 0 && settings.ToolOutputMaxBytes < MinCompactionOverflowToolOutputMaxBytes {
		return CompactionOverflow{}, configError("%s.tool_output_max_bytes must be 0 (off) or between %d and %d", prefix, MinCompactionOverflowToolOutputMaxBytes, maxCompactionOverflowToolOutputMaxBytes)
	}
	return settings, nil
}
