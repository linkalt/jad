package session

import (
	"encoding/json"
	"fmt"
	"math"
)

// RunTokenLimitError is returned when a run has used the max_run_tokens of its
// setting and the model asked for another round. Its text holds only numbers the
// harness counted, so a client may read it; it matches the TypeScript backend.
type RunTokenLimitError struct {
	Used  int64
	Limit int64
}

func (e *RunTokenLimitError) Error() string {
	return fmt.Sprintf("The run stopped before its next round: it used %d tokens, which reached the max_run_tokens limit of %d.", e.Used, e.Limit)
}

// ResponseTokens is responseTokens for a caller outside the package, such as a
// sub-agent run that reports the tokens it used.
func ResponseTokens(event json.RawMessage) int64 { return responseTokens(event) }

// responseTokens is what one provider response reports: its total_tokens, or
// input plus output when no valid total is given. A response without usable usage
// counts as 0, because the harness only counts what the provider reported.
func responseTokens(event json.RawMessage) int64 {
	var envelope struct {
		Response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(event, &envelope) != nil {
		return 0
	}
	return usageTokens(envelope.Response.Usage)
}

// usageTokens is responseTokens for an already decoded usage object.
func usageTokens(usage map[string]json.RawMessage) int64 {
	if usage == nil {
		return 0
	}
	count := func(name string) (int64, bool) {
		raw, exists := usage[name]
		if !exists {
			return 0, false
		}
		value, ok := jsonNumberFloat(raw)
		if !ok || value < 0 || value != math.Trunc(value) || value > 9_007_199_254_740_991 {
			return 0, false
		}
		return int64(value), true
	}
	if total, ok := count("total_tokens"); ok {
		return total
	}
	input, inputOK := count("input_tokens")
	output, outputOK := count("output_tokens")
	if !inputOK || !outputOK {
		return 0
	}
	return input + output
}
