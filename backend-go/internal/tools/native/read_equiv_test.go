package native

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func oldLineSpans(data []byte) []lineSpan {
	spans := make([]lineSpan, 0)
	start := 0
	for cursor := 0; cursor < len(data); {
		if data[cursor] != '\n' && data[cursor] != '\r' {
			cursor++
			continue
		}
		end := cursor
		fullEnd := cursor + 1
		if data[cursor] == '\r' && fullEnd < len(data) && data[fullEnd] == '\n' {
			fullEnd++
		}
		spans = append(spans, lineSpan{start: start, end: end, fullEnd: fullEnd})
		start = fullEnd
		cursor = fullEnd
	}
	if start < len(data) {
		spans = append(spans, lineSpan{start: start, end: len(data), fullEnd: len(data)})
	}
	return spans
}

// scanLines must agree with the whole-file lineSpans: same line count and, for the
// window it returns, the same spans, for any offset, limit and byte cap.
func TestScanLinesMatchesLineSpans(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	pieces := []string{"a", "bb", "\n", "\r\n", "\r", "\n\n", "long line of text", ""}
	for trial := 0; trial < 3000; trial++ {
		var builder strings.Builder
		for count := random.Intn(40); count > 0; count-- {
			builder.WriteString(pieces[random.Intn(len(pieces))])
		}
		data := []byte(builder.String())
		spans := oldLineSpans(data)
		first := int64(random.Intn(len(spans) + 3))
		count := int64(random.Intn(len(spans) + 3))
		maxBytes := int64(random.Intn(60))
		total, window := scanLines(data, first, count, maxBytes)
		if total != int64(len(spans)) {
			t.Fatalf("trial %d: total %d, want %d for %q", trial, total, len(spans), data)
		}
		var want []lineSpan
		var used int64
		for index := first; index < int64(len(spans)) && int64(len(want)) < count && used <= maxBytes; index++ {
			want = append(want, spans[index])
			used += int64(spans[index].fullEnd - spans[index].start)
		}
		if !reflect.DeepEqual(append([]lineSpan(nil), window...), append([]lineSpan(nil), want...)) {
			t.Fatalf("trial %d: window %v, want %v (first %d count %d max %d) for %q", trial, window, want, first, count, maxBytes, data)
		}
	}
}
