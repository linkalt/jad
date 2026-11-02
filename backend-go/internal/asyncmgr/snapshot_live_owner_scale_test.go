package asyncmgr

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// snapshotLiveSkipUnlessMeasured mirrors scaleSkipUnlessMeasured in httpapi/registry_scale_test.go.
// It is a local copy because that helper is unexported in another package.
func snapshotLiveSkipUnlessMeasured(t *testing.T) {
	t.Helper()
	if os.Getenv("JAD_SCALE_MEASURE") != "1" {
		t.Skip("scale measurement: set JAD_SCALE_MEASURE=1 to run")
	}
}

// snapshotLiveDurations returns the nearest-rank percentile of the durations.
func snapshotLiveDurations(values []time.Duration, percent int) time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := max((len(sorted)*percent+99)/100-1, 0)
	return sorted[index]
}

// snapshotLiveOwnerRun registers live handles for one owner on the real manager, then times
// SnapshotLive for that owner. Each call takes m.mu, so the elapsed time bounds the hold.
func snapshotLiveOwnerRun(t *testing.T, live, samples int) {
	t.Helper()
	f := newFixture(t, func(c *Config) {
		c.MaxHandles, c.MaxHandlesTotal = int64(live), int64(live)
		c.KeepFinished, c.UnreadMax = int64(live), int64(live)
		// Every handle is live. The read limit is the product default, so the sort runs over all
		// live rows and only sse_max_ids of them are kept.
	})
	f.driver.autoComplete = false
	for i := range live {
		f.start(t, "owner", fmt.Sprintf("h%06d", i))
	}
	holds := make([]time.Duration, 0, samples)
	for range samples {
		start := time.Now()
		counts, ids := f.m.SnapshotLive("owner")
		holds = append(holds, time.Since(start))
		if counts.Active != live || len(ids) != int(f.m.Config().SSEMaxIDs) {
			t.Fatalf("live=%d: counts %+v ids %d", live, counts, len(ids))
		}
	}
	t.Logf("scale-snapshot-live live_per_owner=%d samples=%d hold_p50=%s hold_p99=%s sse_max_ids=%d",
		live, samples, snapshotLiveDurations(holds, 50), snapshotLiveDurations(holds, 99), f.m.Config().SSEMaxIDs)
	f.m.Close()
}

// TestScaleSnapshotLiveHoldPerOwner measures how long SnapshotLive holds m.mu for one owner
// with 100 and 1000 live handles, on the real manager. It is gated by JAD_SCALE_MEASURE=1.
func TestScaleSnapshotLiveHoldPerOwner(t *testing.T) {
	snapshotLiveSkipUnlessMeasured(t)
	for _, live := range []int{100, 1000} {
		snapshotLiveOwnerRun(t, live, 2000)
	}
}
