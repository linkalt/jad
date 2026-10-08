package anthropicadapter

import "encoding/json"

// Anthropic's thinking blocks must be sent back unchanged, signature included,
// when tools are used. A Responses reasoning item has no field for that, so the
// blocks travel JSON-encoded in encrypted_content, which the caller stores and
// replays without reading.

// isVerifiableThinking reports whether a block can be replayed: a thinking
// block needs its signature, a redacted block needs its data.
func isVerifiableThinking(block *anthropicBlock) bool {
	switch block.Type {
	case "thinking":
		return block.Signature != ""
	case "redacted_thinking":
		return block.Data != ""
	}
	return false
}

// encodeThinkingBlocks packs the replayable blocks. It returns "" when none can
// be replayed.
func encodeThinkingBlocks(blocks []anthropicBlock) string {
	kept := make([]anthropicBlock, 0, len(blocks))
	for _, block := range blocks {
		if !isVerifiableThinking(&block) {
			continue
		}
		if block.Type == "thinking" && block.Thinking == nil {
			empty := ""
			block.Thinking = &empty
		}
		kept = append(kept, anthropicBlock{
			Type: block.Type, Thinking: block.Thinking, Signature: block.Signature, Data: block.Data,
		})
	}
	if len(kept) == 0 {
		return ""
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// decodeThinkingBlocks is the inverse. Content that is not ours, or holds no
// replayable block, yields nothing, so a foreign reasoning item is dropped
// rather than sent to Anthropic as an invalid block.
func decodeThinkingBlocks(encrypted string) []anthropicBlock {
	if encrypted == "" {
		return nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal([]byte(encrypted), &blocks); err != nil {
		return nil
	}
	kept := blocks[:0]
	for _, block := range blocks {
		if !isVerifiableThinking(&block) {
			continue
		}
		if block.Type == "thinking" && block.Thinking == nil {
			empty := ""
			block.Thinking = &empty
		}
		kept = append(kept, block)
	}
	return kept
}
