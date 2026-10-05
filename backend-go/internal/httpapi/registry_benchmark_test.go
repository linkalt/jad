package httpapi

import "testing"

func BenchmarkSSEDeltaEventEncoding(b *testing.B) {
	data := map[string]any{"id": "web-00000000-0000-4000-8000-000000000000", "index": 0, "append": "response text delta"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := makeSSEEvent("ui.delta", data); err != nil {
			b.Fatal(err)
		}
	}
}
