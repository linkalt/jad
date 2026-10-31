package sessionfs

import (
	"encoding/json"
	"testing"
)

// The leaf settings share one integer rule: a plain integer literal only, so
// 1e3 and 10.5 are refused the same way by every node.
func TestLeafIntegerNodesRefuseExponentAndFractionTheSameWay(t *testing.T) {
	for _, raw := range []string{`1e1`, `1E1`, `10.5`, `"10"`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := normalizeHTTPDebugSettings(json.RawMessage(`{"max_body_bytes":`+raw+`}`), "p"); err == nil {
				t.Errorf("http_debug.max_body_bytes %s accepted, want refused", raw)
			}
			if _, err := normalizeImageStoreSettings(json.RawMessage(`{"min_bytes":`+raw+`}`), "/s", "p"); err == nil {
				t.Errorf("image_store.min_bytes %s accepted, want refused", raw)
			}
			if _, err := normalizeSessionFileListing(json.RawMessage(`{"max_depth":`+raw+`}`), "p"); err == nil {
				t.Errorf("session_files.max_depth %s accepted, want refused", raw)
			}
		})
	}
}
