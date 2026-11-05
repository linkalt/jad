package sessionfs

import (
	"encoding/json"
	"strings"
)

// CompactionSummary is compaction.summary: before a compaction drops turns, the session's
// model writes a summary of them, kept in front of the retained turns. Off by default.
type CompactionSummary struct {
	Enabled         bool
	Prompt          string
	Header          string
	Role            string
	MaxInputBytes   int64
	MaxItemBytes    int64
	MaxOutputTokens int64
	TimeoutMS       int64
	OnFailure       string
}

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
}

const (
	defaultCompactionSummaryPrompt = "You are compacting a long conversation between a user and a coding agent so the agent can continue the work with less context. " +
		"Write a summary of the transcript you are given that lets the agent carry on without it. Keep: the user's goals and requests (quote exact instructions and constraints), " +
		"decisions made and why, files read or changed with what matters about them, commands run and their outcomes, errors and how they were resolved, " +
		"open problems, and the work that remains. Keep an earlier summary's facts unless the transcript supersedes them. " +
		"Be specific (paths, names, values), leave out pleasantries, and write only the summary."
	defaultCompactionSummaryHeader          = "Summary of the earlier conversation, written by the model when it was compacted:"
	defaultCompactionSummaryRole            = "developer"
	defaultCompactionSummaryMaxInputBytes   = 200_000
	maxCompactionSummaryMaxInputBytes       = 10_000_000
	defaultCompactionSummaryMaxItemBytes    = 4_000
	maxCompactionSummaryMaxItemBytes        = 1_000_000
	defaultCompactionSummaryMaxOutputTokens = 4_096
	maxCompactionSummaryMaxOutputTokens     = 1_000_000
	defaultCompactionSummaryTimeoutMS       = 120_000
	maxCompactionSummaryTimeoutMS           = 1_800_000
	defaultCompactionSummaryOnFailure       = "compact"

	defaultCompactionOverflowEnabled     = false
	defaultCompactionOverflowMaxAttempts = 1
	maxCompactionOverflowMaxAttempts     = 5
	maxCompactionOverflowPatterns        = 64
)

var (
	compactionSummaryKeys  = []string{"enabled", "prompt", "header", "role", "max_input_bytes", "max_item_bytes", "max_output_tokens", "timeout_ms", "on_failure"}
	compactionOverflowKeys = []string{"enabled", "max_attempts", "patterns"}
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

// DefaultCompactionSummary returns compaction.summary with every default.
func DefaultCompactionSummary() CompactionSummary {
	return CompactionSummary{
		Prompt:          defaultCompactionSummaryPrompt,
		Header:          defaultCompactionSummaryHeader,
		Role:            defaultCompactionSummaryRole,
		MaxInputBytes:   defaultCompactionSummaryMaxInputBytes,
		MaxItemBytes:    defaultCompactionSummaryMaxItemBytes,
		MaxOutputTokens: defaultCompactionSummaryMaxOutputTokens,
		TimeoutMS:       defaultCompactionSummaryTimeoutMS,
		OnFailure:       defaultCompactionSummaryOnFailure,
	}
}

// DefaultCompactionOverflow returns compaction.overflow with every default.
func DefaultCompactionOverflow() CompactionOverflow {
	return CompactionOverflow{
		Enabled:     defaultCompactionOverflowEnabled,
		MaxAttempts: defaultCompactionOverflowMaxAttempts,
		Patterns:    append([]string(nil), defaultCompactionOverflowPatterns...),
	}
}

func normalizeCompactionSummary(raw json.RawMessage, path string) (CompactionSummary, error) {
	settings := DefaultCompactionSummary()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return CompactionSummary{}, configError("%s: compaction.summary must be an object", path)
	}
	if err := rejectUnknownKeys(fields, path+": compaction.summary", compactionSummaryKeys); err != nil {
		return CompactionSummary{}, err
	}
	prefix := path + ": compaction.summary"
	var err error
	if settings.Enabled, err = bashBool(fields, "enabled", false, prefix); err != nil {
		return CompactionSummary{}, err
	}
	if settings.Prompt, err = bashString(fields, "prompt", settings.Prompt, prefix, false); err != nil {
		return CompactionSummary{}, err
	}
	if settings.Header, err = bashString(fields, "header", settings.Header, prefix, false); err != nil {
		return CompactionSummary{}, err
	}
	if settings.Role, err = bashString(fields, "role", settings.Role, prefix, false); err != nil {
		return CompactionSummary{}, err
	}
	if settings.Role != "developer" && settings.Role != "user" {
		return CompactionSummary{}, configError("%s.role must be developer or user", prefix)
	}
	if settings.MaxInputBytes, err = configuredInteger(fields, "max_input_bytes", defaultCompactionSummaryMaxInputBytes, 1000, maxCompactionSummaryMaxInputBytes, path, "compaction.summary"); err != nil {
		return CompactionSummary{}, err
	}
	if settings.MaxItemBytes, err = configuredInteger(fields, "max_item_bytes", defaultCompactionSummaryMaxItemBytes, 100, maxCompactionSummaryMaxItemBytes, path, "compaction.summary"); err != nil {
		return CompactionSummary{}, err
	}
	if settings.MaxOutputTokens, err = configuredInteger(fields, "max_output_tokens", defaultCompactionSummaryMaxOutputTokens, 0, maxCompactionSummaryMaxOutputTokens, path, "compaction.summary"); err != nil {
		return CompactionSummary{}, err
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultCompactionSummaryTimeoutMS, 1000, maxCompactionSummaryTimeoutMS, path, "compaction.summary"); err != nil {
		return CompactionSummary{}, err
	}
	if settings.OnFailure, err = bashString(fields, "on_failure", settings.OnFailure, prefix, false); err != nil {
		return CompactionSummary{}, err
	}
	if settings.OnFailure != "compact" && settings.OnFailure != "skip" {
		return CompactionSummary{}, configError("%s.on_failure must be compact or skip", prefix)
	}
	return settings, nil
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
	return settings, nil
}
