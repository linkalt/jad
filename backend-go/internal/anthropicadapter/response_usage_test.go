package anthropicadapter

import "testing"

func TestTranslateUsageCacheCreationSplit(t *testing.T) {
	split := translateUsage(&anthropicUsage{InputTokens: 1, CacheCreationInputTokens: 10,
		CacheCreation: &cacheCreationUsage{Ephemeral5mInputTokens: 4, Ephemeral1hInputTokens: 6}})
	details := split.InputTokensDetails
	if details["cache_creation_tokens"] != 10 || details["cache_creation_5m_tokens"] != 4 || details["cache_creation_1h_tokens"] != 6 {
		t.Fatalf("details = %v", details)
	}
	absent := translateUsage(&anthropicUsage{InputTokens: 1, CacheCreationInputTokens: 10}).InputTokensDetails
	if absent["cache_creation_tokens"] != 10 || absent["cache_creation_5m_tokens"] != 0 || absent["cache_creation_1h_tokens"] != 0 {
		t.Fatalf("absent split = %v", absent)
	}
}
