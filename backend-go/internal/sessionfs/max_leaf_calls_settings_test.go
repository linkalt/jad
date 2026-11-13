package sessionfs

import (
	"strconv"
	"strings"
	"testing"
)

func TestMaxLeafCallsDefaultsTo256AndTakesBounds(t *testing.T) {
	if defaults := Defaults(); defaults.MaxLeafCalls != 256 {
		t.Fatalf("default = %d, want 256", defaults.MaxLeafCalls)
	}
	if got := loadServerSetting(t, `{}`); got.MaxLeafCalls != 256 {
		t.Fatalf("unset key = %d, want 256", got.MaxLeafCalls)
	}
	for _, want := range []int{1, 64, 1024} {
		got := loadServerSetting(t, `{"max_leaf_calls":`+strconv.Itoa(want)+`}`)
		if got.MaxLeafCalls != want {
			t.Fatalf("max_leaf_calls %d loaded as %d", want, got.MaxLeafCalls)
		}
	}
}

func TestMaxLeafCallsRefusesOutOfRangeAndNonIntegersAndReadsNullAsAbsent(t *testing.T) {
	for _, value := range []string{`0`, `1025`, `"4"`, `1.5`} {
		cwd := t.TempDir()
		writeServerSetting(t, cwd, `{"max_leaf_calls":`+value+`}`)
		_, err := loadServerSection(cwd)
		if err == nil || !strings.Contains(err.Error(), "server.max_leaf_calls") {
			t.Fatalf("value %s: error = %v, want server.max_leaf_calls refused", value, err)
		}
	}
	// Like every other key of the server section, null means the key is absent.
	if got := loadServerSetting(t, `{"max_leaf_calls":null}`); got.MaxLeafCalls != 256 {
		t.Fatalf("null = %d, want the default 256", got.MaxLeafCalls)
	}
}
