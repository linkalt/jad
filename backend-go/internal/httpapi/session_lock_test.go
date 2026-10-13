package httpapi

import (
	"strings"
	"testing"
	"time"
)

type spellingResolver struct{ *fakeResolver }

func (spellingResolver) CanonicalSessionReference(reference string) (string, error) {
	return strings.TrimPrefix(reference, "local:"), nil
}

func TestLockSessionExcludesEverySpellingOfOneSession(t *testing.T) {
	handler := testHandler(t, spellingResolver{&fakeResolver{}}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	t.Cleanup(handler.Close)
	release := handler.lockSession("demo")
	acquired := make(chan func())
	go func() { acquired <- handler.lockSession("local:demo") }()
	select {
	case <-acquired:
		t.Fatal("local:demo got the lock while demo held it")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case other := <-acquired:
		other()
	case <-time.After(2 * time.Second):
		t.Fatal("local:demo never got the lock after demo released it")
	}
}

func TestLockSessionKeepsSeparateSessionsApart(t *testing.T) {
	handler := testHandler(t, spellingResolver{&fakeResolver{}}, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	t.Cleanup(handler.Close)
	release := handler.lockSession("demo")
	defer release()
	acquired := make(chan func())
	go func() { acquired <- handler.lockSession("other") }()
	select {
	case other := <-acquired:
		other()
	case <-time.After(2 * time.Second):
		t.Fatal("another session waited for the lock of demo")
	}
}
