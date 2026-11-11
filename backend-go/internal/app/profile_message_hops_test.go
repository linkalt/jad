package app

import (
	"context"
	"testing"

	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// A local message hands the caller's remote hop count to the target run unchanged: the plan
// takes it from the request's CallerContext, the one carrier since the context value went.
func TestProfileMessageHandsTheCallersRemoteHopsToTheTargetRun(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	for _, hops := range []int{0, 2} {
		request := native.ProfileMessageRequest{
			OwnerKey: profileMessageOwner,
			Target:   "review/reviewer",
			Message:  "hi",
			Caller:   tools.CallerContext{RemoteHops: hops},
		}
		plan, err := messenger.Prepare(callerContext(t, f, context.Background()), request)
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.(*profileMessagePlan).request(&profileMessageRun{}, false).Hops; got != hops {
			t.Errorf("caller hops %d: target run request hops = %d", hops, got)
		}
	}
}
