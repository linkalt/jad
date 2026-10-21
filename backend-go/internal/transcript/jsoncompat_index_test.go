package transcript

import (
	"strconv"
	"testing"
)

func legacyJavascriptArrayIndex(key string) (uint64, bool) {
	if key == "" || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseUint(key, 10, 32)
	if err != nil || value == 4294967295 || strconv.FormatUint(value, 10) != key {
		return 0, false
	}
	return value, true
}

func TestJavascriptArrayIndexMatchesLegacy(t *testing.T) {
	keys := []string{"", "0", "00", "01", "1", "9", "10", "007", "4294967294", "4294967295", "4294967296", "99999999999999999999",
		"-1", "-0", "+1", " 1", "1 ", "1.0", "1e3", "0x10", "a", "A1", "1a", "_1", "٣", "１", "\x00", "z", "constructor"}
	for _, key := range keys {
		gotValue, gotOK := javascriptArrayIndex(key)
		wantValue, wantOK := legacyJavascriptArrayIndex(key)
		if gotValue != wantValue || gotOK != wantOK {
			t.Errorf("javascriptArrayIndex(%q) = %d, %v; want %d, %v", key, gotValue, gotOK, wantValue, wantOK)
		}
	}
}

func TestOrderJavaScriptKeysWithoutIndexKeysKeepsOrder(t *testing.T) {
	fields := []jsonField{{key: "b", value: []byte("1")}, {key: "a", value: []byte("2")}, {key: "-1", value: []byte("3")}}
	got := orderJavaScriptKeys(fields)
	for index, field := range fields {
		if got[index].key != field.key {
			t.Fatalf("order changed at %d: %q, want %q", index, got[index].key, field.key)
		}
	}
	mixed := orderJavaScriptKeys([]jsonField{{key: "b"}, {key: "10"}, {key: "a"}, {key: "2"}})
	if mixed[0].key != "2" || mixed[1].key != "10" || mixed[2].key != "b" || mixed[3].key != "a" {
		t.Fatalf("mixed order = %v", mixed)
	}
}
