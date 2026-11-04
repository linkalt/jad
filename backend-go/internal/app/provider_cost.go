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
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Response.Usage == nil {
		return 0, false
	}
	usage := envelope.Response.Usage
	return p.pricing.CostUSD(usage.InputTokens, usage.InputTokensDetails.CachedTokens, usage.OutputTokens), true
}
