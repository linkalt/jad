package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"jad/backend-go/internal/httpapi"
)

// recordingProfileRuns is a run registry that records the start it is given and answers with
// run and err.
type recordingProfileRuns struct {
	httpapi.ProfileRunStarter
	got []httpapi.ProfileRunRequest
	ctx context.Context
	run *httpapi.ProfileRun
	err error
}

func (s *recordingProfileRuns) StartProfileRun(ctx context.Context, request httpapi.ProfileRunRequest) (*httpapi.ProfileRun, error) {
	s.ctx = ctx
	s.got = append(s.got, request)
	return s.run, s.err
}

type persistentStartKey struct{}

// TestStartRunHandsAPersistentRunToTheRegistryUnchanged (R5b): a persistent start is the
// registry's start, with the request, the context, the run and the error passed through as they are.
func TestStartRunHandsAPersistentRunToTheRegistryUnchanged(t *testing.T) {
	request := httpapi.ProfileRunRequest{Reference: "local:demo", Profile: "review/reviewer", Caller: "dev/coder", Chain: []string{"a", "b"}, Message: "hi", Model: "openai/small", Hops: 2, Wait: true}
	resolver := &Resolver{}
	registry := &recordingProfileRuns{run: &httpapi.ProfileRun{}}
	resolver.SetProfileRuns(registry)
	ctx := context.WithValue(context.Background(), persistentStartKey{}, "caller")
	handle, err := resolver.StartRun(ctx, RunSpec{Kind: RunPersistent, Profile: request})
	if err != nil || handle.ProfileRun() != registry.run || handle.Result() != (RunResult{}) {
		t.Fatalf("StartRun = %+v, %v; want the registry's run", handle, err)
	}
	if len(registry.got) != 1 || !reflect.DeepEqual(registry.got[0], request) || registry.ctx.Value(persistentStartKey{}) != "caller" {
		t.Fatalf("the registry got %+v (ctx %v), want %+v on the caller's context", registry.got, registry.ctx, request)
	}

	registry.run, registry.err = nil, httpapi.ErrSessionBusy
	if handle, err := resolver.StartRun(ctx, RunSpec{Kind: RunPersistent, Profile: request}); handle != nil || !errors.Is(err, httpapi.ErrSessionBusy) {
		t.Fatalf("a busy target = %+v, %v; want no handle and ErrSessionBusy", handle, err)
	}
}

// Before the HTTP handler sets the registry, a persistent start is refused with the text
// profile_message reported before R5b.
func TestStartRunRefusesAPersistentRunWithoutARegistry(t *testing.T) {
	handle, err := (&Resolver{}).StartRun(context.Background(), RunSpec{Kind: RunPersistent})
	if handle != nil || err == nil || err.Error() != "profile messages are unavailable: the run registry is not set" {
		t.Fatalf("StartRun = %+v, %v", handle, err)
	}
	if handle.ProfileRun() != nil || handle.Result() != (RunResult{}) {
		t.Fatal("a nil handle must answer with nothing")
	}
}
