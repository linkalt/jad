package native

import (
	"strings"
	"testing"
)

func TestReplaceDataURLsLeavesTextWithoutADataURLUntouched(t *testing.T) {
	text := strings.Repeat("plain tool output ", 4096)
	if allocs := testing.AllocsPerRun(10, func() { replaceDataURLs(text) }); allocs != 0 {
		t.Fatalf("text without a data URL allocated %v times; it must be returned as is", allocs)
	}
	if got := replaceDataURLs("see data:image/png;base64," + strings.Repeat("A", 64) + " here"); got != "see [data URL 86 bytes] here" {
		t.Fatalf("data URL not replaced: %q", got)
	}
}
