package tools

import (
	"encoding/json"
	"math"
	"math/big"
)

// SafeInteger reads a JSON integer argument: a json.Number, float64, int or int64 that
// is a whole number a JavaScript number holds exactly.
func SafeInteger(value any) (int64, bool) {
	var rational *big.Rat
	switch typed := value.(type) {
	case json.Number:
		var ok bool
		rational, ok = new(big.Rat).SetString(typed.String())
		if !ok {
			return 0, false
		}
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		if typed > 9_007_199_254_740_991 || typed < -9_007_199_254_740_991 || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
	if !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	integer := rational.Num().Int64()
	if integer > 9_007_199_254_740_991 || integer < -9_007_199_254_740_991 {
		return 0, false
	}
	return integer, true
}
