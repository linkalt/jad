package app

import (
	"crypto/sha256"
	"encoding/hex"

	"jad/backend-go/internal/sessionfs"
)

// promptCacheKeyPrefix starts every prompt_cache_key jad sends, so a provider-side log
// shows where a key came from.
const promptCacheKeyPrefix = "jad-"

// promptCacheKey is the prompt_cache_key of the requests a runner sends: "" when the model
// file leaves it off, otherwise a digest of scope. scope names the transcript (the session's
// messages path, plus the agent name for a sub-agent), so every request of a session carries
// the same key, and the key holds no path or name.
func promptCacheKey(model sessionfs.Model, scope string) string {
	if model.PromptCacheKey != sessionfs.PromptCacheKeySession {
		return ""
	}
	sum := sha256.Sum256([]byte(scope))
	return promptCacheKeyPrefix + hex.EncodeToString(sum[:16])
}
