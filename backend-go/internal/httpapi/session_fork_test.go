package httpapi

import (
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

// The new profile's name and title are bounded by server.fork_name_max_bytes, not by a
// fixed number.
func TestForkNameLimitIsTheConfiguredOne(t *testing.T) {
	for _, test := range []struct {
		body   string
		limit  int
		refuse string
	}{
		{`{"profile":"abcdefghij"}`, 10, ""},
		{`{"profile":"abcdefghijk"}`, 10, "must be at most 10 bytes (server.fork_name_max_bytes)"},
		{`{"profile":"a","title":"0123456789"}`, 10, ""},
		{`{"profile":"a","title":"0123456789x"}`, 10, "title must be one line of 1 to 10 bytes (server.fork_name_max_bytes)"},
		{`{"profile":"` + strings.Repeat("a", 250) + `"}`, 250, ""},
		{`{"profile":"` + strings.Repeat("a", 201) + `"}`, 200, "must be at most 200 bytes"},
	} {
		_, err := parseForkRequest([]byte(test.body), test.limit)
		switch {
		case test.refuse == "" && err != nil:
			t.Errorf("%s with limit %d: %v", test.body, test.limit, err)
		case test.refuse != "" && (err == nil || !strings.Contains(err.Error(), test.refuse)):
			t.Errorf("%s with limit %d: error %v, want %q", test.body, test.limit, err, test.refuse)
		}
	}
}

func TestHandlerTakesTheForkNameLimitFromItsConfig(t *testing.T) {
	config := func(limit int) Config {
		return Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxPageTurns: 1, ShutdownTimeout: 50 * time.Millisecond, ForkNameMaxBytes: limit}
	}
	for limit, want := range map[int]int{0: serverconfig.DefaultForkNameMaxBytes, 7: 7, serverconfig.MaxForkNameMaxBytes: serverconfig.MaxForkNameMaxBytes} {
		handler, err := NewHandler(config(limit), &fakeResolver{})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		handler.Close()
		if handler.forkNameMaxBytes != want {
			t.Fatalf("limit %d: handler limit %d, want %d", limit, handler.forkNameMaxBytes, want)
		}
	}
	for _, limit := range []int{-1, serverconfig.MaxForkNameMaxBytes + 1} {
		if _, err := NewHandler(config(limit), &fakeResolver{}); err == nil || !strings.Contains(err.Error(), "fork name max bytes") {
			t.Errorf("limit %d: error %v", limit, err)
		}
	}
}
