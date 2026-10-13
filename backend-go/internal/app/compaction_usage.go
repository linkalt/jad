package app

import (
	"fmt"
	"os"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// compactPreservingUsage compacts the transcript without losing the session's
// cumulative usage. The transcript package deletes the derived usage cache
// because compaction rewrites the file it checkpoints; the TypeScript backend
// instead keeps the totals and zeroes only the latest response's token counts.
// This brings the cache current first, compacts, then republishes the cache at
// the new transcript with that reset applied.
func (s *sharedSession) compactPreservingUsage(config sessionfs.SessionConfig, options transcript.CompactionOptions) (transcript.CompactionResult, error) {
	before, err := scanTranscriptIndex(s, config)
	if err != nil {
		return transcript.CompactionResult{}, fmt.Errorf("read usage before compaction: %w", err)
	}
	result, err := s.store.CompactHistory(options)
	if err != nil || !result.Compacted {
		return result, err
	}
	info, statErr := os.Stat(config.MessagesPath)
	if statErr != nil {
		// Without a transcript to checkpoint, the next scan rebuilds the usage.
		return result, nil
	}
	summary := resetLatestResponseUsage(before.usage)
	if err := writeUsageCacheCheckpoint(config, summary, info.Size(), before.lastResponseKey); err != nil {
		return result, fmt.Errorf("write usage cache after compaction: %w", err)
	}
	return result, nil
}

// frontCompactPreservingUsage front-compacts the transcript with the same usage
// rule as compactPreservingUsage: the totals stay and the latest response's
// token counts reset to 0, instead of being rebuilt from the kept messages.
func (s *sharedSession) frontCompactPreservingUsage(config sessionfs.SessionConfig, messageID, deleteMode string) (transcript.FrontCompactionResult, error) {
	before, err := scanTranscriptIndex(s, config)
	if err != nil {
		return transcript.FrontCompactionResult{}, fmt.Errorf("read usage before front compaction: %w", err)
	}
	result, err := s.store.FrontCompactFromMessage(messageID, deleteMode)
	if err != nil {
		return result, err
	}
	info, statErr := os.Stat(config.MessagesPath)
	if statErr != nil {
		return result, nil
	}
	summary := resetLatestResponseUsage(before.usage)
	if err := writeUsageCacheCheckpoint(config, summary, info.Size(), before.lastResponseKey); err != nil {
		return result, fmt.Errorf("write usage cache after front compaction: %w", err)
	}
	return result, nil
}

// resetLatestResponseUsage zeroes the latest response's token counts while
// leaving the cumulative totals intact, matching the post-compaction baseline.
func resetLatestResponseUsage(summary webui.UsageSummary) webui.UsageSummary {
	if summary.Current == nil {
		return summary
	}
	current := *summary.Current
	zero := func() *int64 { value := int64(0); return &value }
	current.InputTokens = zero()
	current.OutputTokens = zero()
	current.TotalTokens = zero()
	current.CachedInputTokens = zero()
	summary.Current = &current
	return summary
}
