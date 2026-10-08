package main

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer is a buffer that a command goroutine writes and a test reads at the same time.
type safeBuffer struct {
	mutex sync.Mutex
	text  strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.Write(p)
}

func (b *safeBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.String()
}

var urlPattern = regexp.MustCompile(`https?://\S+/oauth/authorize\?\S+`)

// waitForURL waits until the command has printed the authorization URL.
func waitForURL(t *testing.T, out *safeBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found := urlPattern.FindString(out.String()); found != "" {
			return found
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the command did not print an authorization URL; output: %q", out.String())
	return ""
}
