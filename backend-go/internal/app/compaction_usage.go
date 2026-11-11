package app

import (
	"fmt"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

// afterRewritePublished is a test seam: it runs after a rewrite replaced the transcript and
// before the cache is republished, the window in which a reader sees no usage cache. It is nil
// in production.
var afterRewritePublished func()

// compactPreservingUsage compacts the transcript without losing the session's
// cumulative usage. The transcript package deletes the derived usage cache
// because compaction rewrites the file it checkpoints; the TypeScript backend
// instead keeps the totals and zeroes only the latest response's token counts.
// This brings the usage current first, stores it as the usage baseline of the
// new transcript before that transcript is published, compacts, then republishes
// the cache at the new transcript with that reset applied. The cache is only a
// shortcut: until it is back, readers continue from the baseline.
func (s *sharedSession) compactPreservingUsage(config sessionfs.SessionConfig, options transcript.CompactionOptions) (transcript.CompactionResult, error) {
	var result transcript.CompactionResult
	err := s.rewritePreservingUsage(config, "compaction", false, func(before func(transcript.Replacement) error) (bool, error) {
		options.BeforePublish = before
		var err error
		result, err = s.store.CompactHistory(options)
		return result.Compacted, err
	})
	return result, err
}

// frontCompactPreservingUsage front-compacts the transcript with the same usage
// rule as compactPreservingUsage: the totals stay and the latest response's
// token counts reset to 0, instead of being rebuilt from the kept messages.
func (s *sharedSession) frontCompactPreservingUsage(config sessionfs.SessionConfig, messageID, deleteMode string) (transcript.FrontCompactionResult, error) {
	var result transcript.FrontCompactionResult
	err := s.rewritePreservingUsage(config, "front compaction", true, func(before func(transcript.Replacement) error) (bool, error) {
		var err error
		result, err = s.store.FrontCompactFromMessageWith(messageID, deleteMode, before)
		return err == nil, err
	})
	return result, err
}

// rewritePreservingUsage runs rewrite, a transcript rewrite that calls the hook it is given
// right before it publishes, so that the totals of the records the rewrite removes stay in the
// usage baseline. keptPrefix says the rewrite publishes a prefix of the transcript (front
// compaction). rewrite reports whether it replaced the transcript. A failure to store the
// baseline fails the rewrite before the transcript changes: the baseline cannot be rebuilt, so
// a rewrite without it would lose those totals for good.
func (s *sharedSession) rewritePreservingUsage(config sessionfs.SessionConfig, what string, keptPrefix bool, rewrite func(before func(transcript.Replacement) error) (bool, error)) error {
	before, err := scanTranscriptIndex(s, config)
	if err != nil {
		return fmt.Errorf("read usage before %s: %w", what, err)
	}
	var published transcript.Replacement
	replaced, err := rewrite(s.usageBaselineHook(config, before, &published, keptPrefix))
	if err != nil || !replaced {
		return err
	}
	if afterRewritePublished != nil {
		afterRewritePublished()
	}
	// The cache covers the published bytes, not whatever was appended since.
	offset := published.Size
	stat, statErr := statFile(config.MessagesPath)
	if statErr != nil {
		// Without a transcript to checkpoint, the next scan continues from the baseline.
		return nil
	}
	if published.SHA256 == "" {
		offset = stat.Size
	}
	s.writeUsageCache(config, resetLatestResponseUsage(before.usage), offset, before.lastResponseKey, stat.generation())
	return nil
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
