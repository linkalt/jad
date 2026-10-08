package anthropicadapter

// applyCache places the prompt-caching breakpoints chosen in the config.
//
// Anthropic caches the prefix tools, system, messages up to each breakpoint and
// allows at most four of them. The four switches use at most four slots, so no
// combination can exceed the limit.
func applyCache(out *anthropicRequest, config *CacheConfig) {
	marker := &cacheControl{Type: "ephemeral", TTL: config.TTL}
	if config.Tools && len(out.Tools) > 0 {
		out.Tools[len(out.Tools)-1].CacheControl = marker
	}
	if config.System && len(out.System) > 0 {
		out.System[len(out.System)-1].CacheControl = marker
	}
	if config.History {
		if block := lastHistoryBlock(out.Messages); block != nil {
			block.CacheControl = marker
		}
	}
	if config.Automatic {
		out.CacheControl = marker
	}
}

// lastHistoryBlock returns the last block that carries conversation content
// and can hold a breakpoint. It skips text folded in from system or developer
// messages, mid-conversation system messages, thinking blocks (which cannot be
// marked) and empty text (which cannot be cached).
func lastHistoryBlock(messages []anthropicMessage) *anthropicBlock {
	for m := len(messages) - 1; m >= 0; m-- {
		if messages[m].Role == "system" {
			continue
		}
		for b := len(messages[m].Content) - 1; b >= 0; b-- {
			block := &messages[m].Content[b]
			if block.folded || !canCache(block) {
				continue
			}
			return block
		}
	}
	return nil
}

func canCache(block *anthropicBlock) bool {
	switch block.Type {
	case "thinking", "redacted_thinking":
		return false
	case "text":
		return block.Text != ""
	}
	return true
}
