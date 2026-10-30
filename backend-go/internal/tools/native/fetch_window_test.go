package native

import (
	"math/rand"
	"testing"
)

// fetchWindow must return what sliceFetchCharacters returns for every offset and limit.
func TestFetchWindowMatchesSliceFetchCharacters(t *testing.T) {
	pieces := []string{"a", "é", "中", "😀", "\xff", "\xf0\x9f", "z"}
	random := rand.New(rand.NewSource(1))
	for round := 0; round < 400; round++ {
		value := ""
		for i := random.Intn(12); i >= 0; i-- {
			value += pieces[random.Intn(len(pieces))]
		}
		total := int64(utf16Length(value))
		for offset := int64(0); offset <= total+1; offset++ {
			for _, limit := range []int64{0, 1, 2, 3, 5, total, total + 4, 1 << 62} {
				wantText, wantConsumed := sliceFetchCharacters(value, offset, limit)
				gotText, gotConsumed, gotTotal := fetchWindow(value, offset, limit)
				if gotText != wantText || gotConsumed != wantConsumed || gotTotal != total {
					t.Fatalf("value %q offset %d limit %d: window (%q, %d, %d), want (%q, %d, %d)",
						value, offset, limit, gotText, gotConsumed, gotTotal, wantText, wantConsumed, total)
				}
			}
		}
	}
}
