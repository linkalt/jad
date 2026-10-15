package httpapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/openaiapi"
)

func TestClientRunErrorMessageNamesAnIdleProviderWithoutDetail(t *testing.T) {
	wrapped := fmt.Errorf("run: %w", &openaiapi.StreamIdleError{Timeout: 300 * time.Second})
	got := clientRunErrorMessage(wrapped)
	if !strings.Contains(got, "5m0s") || !strings.Contains(got, "server log") {
		t.Fatalf("message = %q, want the idle time and a pointer to the log", got)
	}
	if got == clientRunErrorMessage(errors.New("x")) {
		t.Fatalf("message = %q, want it to differ from the generic failure", got)
	}
}
