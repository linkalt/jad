package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// benchmarkNestedStructuredContent builds {"a":{"a":...{"a":"<leaf>"}...}} of the given
// nesting depth, the shape that made the per-level re-encode of structured content
// visible (P6AM-02).
func benchmarkNestedStructuredContent(depth, leafBytes int) json.RawMessage {
	var builder strings.Builder
	builder.Grow(depth*7 + leafBytes)
	for level := 0; level < depth; level++ {
		builder.WriteString(`{"a":`)
	}
	builder.WriteString(`"`)
	builder.WriteString(strings.Repeat("x", leafBytes))
	builder.WriteString(`"`)
	for level := 0; level < depth; level++ {
		builder.WriteString(`}`)
	}
	return json.RawMessage(builder.String())
}

// BenchmarkPreserveStructuredContentOrder measures the production conversion of struct
// content: preserveStructuredContentOrder followed by compactJSON, as executeTool runs
// it. The curve per depth shows whether the encoding is linear in the payload or
// repeated at every nesting level.
func BenchmarkPreserveStructuredContentOrder(b *testing.B) {
	for _, depth := range []int{3, 50, 200, 1000} {
		raw := benchmarkNestedStructuredContent(depth, 1<<20)
		b.Run(fmt.Sprintf("depth%d", depth), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				ordered, err := preserveStructuredContentOrder(raw)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := compactJSON(ordered); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
