package extension

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// The base64 text is judged by its length first, so a very long string is not decoded.
func TestDecideImageDataIsRefusedFromItsLengthBeforeDecoding(t *testing.T) {
	// Not valid base64 and far over the limit: the length check must answer first, so the
	// reason is the size and not an invalid-data message.
	_, failure := decodeDecideImageData(strings.Repeat("!", 100000), sessionfs.ExtensionDecideSettings{MaxImageBytes: 1000}, "c", "input part 1")
	if !strings.Contains(failure, "image_too_large") {
		t.Fatalf("failure = %q", failure)
	}
}
