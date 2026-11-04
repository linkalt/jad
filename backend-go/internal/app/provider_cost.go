package app

import (
	"context"
	"encoding/json"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
)

// pricedStreamer adds the cost of each response its client streams to the cost meter of
// the run's context, at the pricing of its own model. Each provider client gets its own,
// so a response a fallback model answered is priced as that model.
type pricedStreamer struct {
	inner   responses.Streamer
	pricing sessionfs.ModelPricing
}

func withPricing(streamer responses.Streamer, model sessionfs.Model) responses.Streamer {
	if model.Pricing == nil {
		return streamer
	}
	return &pricedStreamer{inner: streamer, pricing: *model.Pricing}
}

func (p *pricedStreamer) Stream(ctx context.Context, body json.RawMessage, handle responses.EventHandler) error {
	meter := session.CostMeterFrom(ctx)
	if meter == nil {
		return p.inner.Stream(ctx, body, handle)
	}
	return p.inner.Stream(ctx, body, func(event responses.Event) error {
		switch event.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if cost, ok := p.responseCost(event.Raw); ok {
				meter.Add(cost)
			}
		}
		return handle(event)
	})
}

// responseCost prices the usage a final response event reports; a response without usage
// costs nothing that can be counted.
func (p *pricedStreamer) responseCost(raw json.RawMessage) (float64, bool) {
	var envelope struct {
		Response struct {
			Usage *struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
					// The Anthropic translation reports cache writes; other providers leave
					// them out, and then nothing is priced as a write.
					CacheCreationTokens   int64 `json:"cache_creation_tokens"`
					CacheCreation5mTokens int64 `json:"cache_creation_5m_tokens"`
					CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Response.Usage == nil {
		return 0, false
	}
	usage := envelope.Response.Usage
	details := usage.InputTokensDetails
	write5m, write1h := details.CacheCreation5mTokens, details.CacheCreation1hTokens
	// A total without the split (or with a split that misses part of it) counts the rest as
	// 5-minute writes, the default lifetime.
	if rest := details.CacheCreationTokens - write5m - write1h; rest > 0 {
		write5m += rest
	}
	return p.pricing.Cost(sessionfs.TokenUsage{
		InputTokens: usage.InputTokens, CachedTokens: details.CachedTokens,
		CacheWriteTokens: write5m, CacheWrite1hTokens: write1h, OutputTokens: usage.OutputTokens,
	}), true
}
