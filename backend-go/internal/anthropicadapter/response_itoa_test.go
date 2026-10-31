package anthropicadapter

import (
	"strconv"
	"testing"
)

func TestItoaMatchesStrconv(t *testing.T) {
	for _, value := range []int{0, 1, -1, 42, -987654, 1 << 40} {
		if got, want := itoa(value), strconv.Itoa(value); got != want {
			t.Errorf("itoa(%d) = %q, want %q", value, got, want)
		}
	}
}
