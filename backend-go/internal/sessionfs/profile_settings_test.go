package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProfileRefusesTheRemovedHistoryCacheKey(t *testing.T) {
	err := validateProfileFields("a", json.RawMessage(`{"history_cache":"x.json"}`), "setting.json: paths.profiles.a")
	if err == nil || !strings.Contains(err.Error(), "paths.profiles.a.history_cache is not a supported session path or profile model setting") {
		t.Fatalf("error = %v", err)
	}
	if err := validateProfileFields("a", json.RawMessage(`{"usage_cache":"x.json"}`), "setting.json: paths.profiles.a"); err != nil {
		t.Fatalf("a kept key must still load: %v", err)
	}
}
