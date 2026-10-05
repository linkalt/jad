package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const maxSafeJSONInteger int64 = 9007199254740991

// AddCompletedResponse updates a cumulative summary from one persisted
// response.completed record. seenResponseKeys prevents duplicate records from
// inflating totals when a transcript contains a repeated response ID.
func AddCompletedResponse(summary *UsageSummary, raw json.RawMessage, seenResponseKeys map[string]struct{}) error {
	if summary == nil {
		return fmt.Errorf("usage summary is required")
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil || record == nil {
		return fmt.Errorf("usage record must be a JSON object")
	}
	if stringField(record["type"]) != "response.completed" {
		return nil
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(record["response"], &response); err != nil || response == nil {
		return fmt.Errorf("completed response record must contain an object response")
	}
	responseID := stringField(response["id"])
	key := "response:" + responseID
	if responseID == "" {
		digest := sha256.Sum256(raw)
		key = hex.EncodeToString(digest[:])
	}
	if seenResponseKeys != nil {
		if _, exists := seenResponseKeys[key]; exists {
			return nil
		}
		seenResponseKeys[key] = struct{}{}
	}

	metrics := parseTokenUsage(response["usage"])
	hasUsage := metrics.InputTokens != nil || metrics.OutputTokens != nil || metrics.TotalTokens != nil || metrics.CachedInputTokens != nil
	turn := positiveJSONInteger(record["session_turn"])
	if turn == 0 {
		turn = summary.TurnCount
		if turn == 0 {
			turn = 1
		}
	}
	if turn > summary.TurnCount {
		summary.TurnCount = turn
	}
	step := positiveJSONInteger(record["session_step"])
	if step == 0 {
		step = 1
		if summary.Current != nil && summary.Current.Turn == turn {
			step = summary.Current.Step + 1
		}
	}

	summary.CompletedResponses++
	if hasUsage {
		summary.ResponsesWithUsage++
		addUsage(&summary.TokenUsage, metrics)
	}
	current := &CurrentResponseUsage{TokenUsage: metrics, Turn: turn, Step: step}
	if responseID != "" {
		current.ResponseID = responseID
	}
	summary.Current = current
	return nil
}

func parseTokenUsage(raw json.RawMessage) TokenUsage {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil || usage == nil {
		return TokenUsage{}
	}
	metrics := TokenUsage{
		InputTokens:  nonnegativeJSONInteger(usage["input_tokens"]),
		OutputTokens: nonnegativeJSONInteger(usage["output_tokens"]),
		TotalTokens:  nonnegativeJSONInteger(usage["total_tokens"]),
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(usage["input_tokens_details"], &details) == nil && details != nil {
		metrics.CachedInputTokens = nonnegativeJSONInteger(details["cached_tokens"])
	}
	if metrics.TotalTokens == nil && metrics.InputTokens != nil && metrics.OutputTokens != nil {
		total := *metrics.InputTokens + *metrics.OutputTokens
		if total <= maxSafeJSONInteger {
			metrics.TotalTokens = &total
		}
	}
	return metrics
}

func addUsage(total *TokenUsage, increment TokenUsage) {
	total.InputTokens = addTokenCount(total.InputTokens, increment.InputTokens)
	total.OutputTokens = addTokenCount(total.OutputTokens, increment.OutputTokens)
	total.TotalTokens = addTokenCount(total.TotalTokens, increment.TotalTokens)
	total.CachedInputTokens = addTokenCount(total.CachedInputTokens, increment.CachedInputTokens)
}

func addTokenCount(current, add *int64) *int64 {
	if add == nil {
		return current
	}
	if current == nil {
		value := *add
		return &value
	}
	value := *current + *add
	if value > maxSafeJSONInteger {
		return current
	}
	return &value
}

func nonnegativeJSONInteger(raw json.RawMessage) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 || value > maxSafeJSONInteger {
		return nil
	}
	return &value
}

func positiveJSONInteger(raw json.RawMessage) int64 {
	value := nonnegativeJSONInteger(raw)
	if value == nil || *value == 0 {
		return 0
	}
	return *value
}
