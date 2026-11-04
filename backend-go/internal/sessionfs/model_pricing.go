package sessionfs

import (
	"encoding/json"
	"math"
	"strings"
)

// maxModelPricePerMTok bounds a price, so a typo of a few orders of magnitude is caught.
const maxModelPricePerMTok = 100_000.0

var modelPricingKeys = []string{"input_per_mtok", "cached_input_per_mtok", "output_per_mtok"}

// ModelPricing is what a model costs, in US dollars per million tokens.
type ModelPricing struct {
	InputPerMTok       float64
	CachedInputPerMTok float64
	OutputPerMTok      float64
}

// CostUSD is the price of one response's usage: cached input at its own price, the rest
// of the input at the input price, and the output (reasoning included) at the output price.
func (p ModelPricing) CostUSD(inputTokens, cachedTokens, outputTokens int64) float64 {
	cachedTokens = min(max(cachedTokens, 0), max(inputTokens, 0))
	fresh := max(inputTokens, 0) - cachedTokens
	return (float64(fresh)*p.InputPerMTok + float64(cachedTokens)*p.CachedInputPerMTok + float64(max(outputTokens, 0))*p.OutputPerMTok) / 1_000_000
}

// modelPricingField reads a model file's pricing; absent or null is nil (unknown cost).
// cached_input_per_mtok defaults to the input price.
func modelPricingField(fields map[string]json.RawMessage, path string) (*ModelPricing, error) {
	raw, exists := fields["pricing"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	values, err := decodeConfigObject(raw, path+": pricing")
	if err != nil {
		return nil, configError("%s: pricing must be an object", path)
	}
	for key := range values {
		if !containsString(modelPricingKeys, key) {
			return nil, unsupportedToolKey(path+": pricing", key, modelPricingKeys, "")
		}
	}
	price := func(name string) (float64, bool, error) {
		raw, exists := values[name]
		if !exists {
			return 0, false, nil
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || value < 0 || value > maxModelPricePerMTok {
			return 0, false, configError("%s: pricing.%s must be a number of US dollars per million tokens between 0 and %g", path, name, maxModelPricePerMTok)
		}
		return value, true, nil
	}
	pricing := &ModelPricing{}
	var has bool
	if pricing.InputPerMTok, has, err = price("input_per_mtok"); err != nil {
		return nil, err
	} else if !has {
		return nil, configError("%s: pricing needs input_per_mtok and output_per_mtok", path)
	}
	if pricing.OutputPerMTok, has, err = price("output_per_mtok"); err != nil {
		return nil, err
	} else if !has {
		return nil, configError("%s: pricing needs input_per_mtok and output_per_mtok", path)
	}
	if pricing.CachedInputPerMTok, has, err = price("cached_input_per_mtok"); err != nil {
		return nil, err
	} else if !has {
		pricing.CachedInputPerMTok = pricing.InputPerMTok
	}
	return pricing, nil
}

// normalizeMaxRunCost reads max_run_cost_usd; absent means no cap (0).
func normalizeMaxRunCost(raw json.RawMessage, path string) (float64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || value <= 0 || value > 1_000_000 {
		return 0, configError("%s: max_run_cost_usd must be a number of US dollars above 0 and at most 1000000", path)
	}
	return value, nil
}
