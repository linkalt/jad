package native

import (
	"math/rand"
	"testing"
	"unicode/utf8"
)

type oldUTF8Tracker struct {
	pending []byte
	invalid bool
}

func (tracker *oldUTF8Tracker) push(chunk []byte) {
	input := make([]byte, 0, len(tracker.pending)+len(chunk))
	input = append(input, tracker.pending...)
	input = append(input, chunk...)
	tracker.pending = nil
	for len(input) > 0 {
		if !utf8.FullRune(input) {
			tracker.pending = append(tracker.pending[:0], input...)
			return
		}
		r, size := utf8.DecodeRune(input)
		if r == utf8.RuneError && size == 1 {
			tracker.invalid = true
		}
		input = input[size:]
	}
}

func (tracker *oldUTF8Tracker) end() {
	if len(tracker.pending) > 0 {
		tracker.invalid = true
		tracker.pending = nil
	}
}

// The collector's binary flag must not change: same verdict as the previous tracker for
// any bytes split into any chunks.
func TestStreamCollectorBinaryVerdictMatchesPreviousTracker(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	pieces := [][]byte{[]byte("abc"), []byte("é"), []byte("€"), []byte("😀"), {0xff}, {0xc3}, {0xe2, 0x82}, {0xf0, 0x9f}, {0x80}, {0}, []byte("\n")}
	for trial := 0; trial < 20000; trial++ {
		var data []byte
		for count := random.Intn(12); count > 0; count-- {
			data = append(data, pieces[random.Intn(len(pieces))]...)
		}
		old := &oldUTF8Tracker{}
		collector := newBashStreamCollector(bashStreamLimits{headBytes: 4, tailBytes: 4})
		oldBinary := false
		for rest := data; len(rest) > 0; {
			size := 1 + random.Intn(len(rest))
			chunk := append([]byte(nil), rest[:size]...)
			rest = rest[size:]
			for _, value := range chunk {
				if value == 0 {
					oldBinary = true
				}
			}
			old.push(chunk)
			collector.push(chunk)
		}
		old.end()
		collector.end()
		if old.invalid {
			oldBinary = true
		}
		if collector.binary != oldBinary {
			t.Fatalf("trial %d: binary = %v, previous tracker says %v for %q", trial, collector.binary, oldBinary, data)
		}
		if got := collector.producedBytes; got != int64(len(data)) {
			t.Fatalf("trial %d: producedBytes = %d, want %d", trial, got, len(data))
		}
	}
}

func BenchmarkStreamCollectorPush(b *testing.B) {
	chunk := make([]byte, 32*1024)
	for index := range chunk {
		chunk[index] = "output line of a build log\n"[index%27]
	}
	collector := newBashStreamCollector(bashStreamLimits{headBytes: 750, tailBytes: 1200})
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		collector.push(chunk)
	}
}
