package native

import (
	"context"
	"strconv"
	"sync/atomic"
)

// A run's lineage is the keys of the runs and owners above it, across both kinds of run: the
// lineage key (NewRunLineageKey) of every sub-agent run it is nested in and of the persistent run
// that started the tree, and, for a run a profile_message started, the session key of the calling
// profile and the key of the message's own run in the target profile (app). It travels
// as a value on the run's context and is recorded on every handle and sub-agent run started under
// it, so a stop, reset or delete of any of those keys reaches the work a chain left going in other
// profiles and sessions (Resolver.cancelLineage), not only what its owner fan-out names. It is
// held in memory only: records and manifests do not carry it, and a restart marks the work lost.
type runLineageKey struct{}

// WithRunLineage puts lineage on ctx. An empty lineage leaves ctx as it is.
func WithRunLineage(ctx context.Context, lineage []string) context.Context {
	if len(lineage) == 0 {
		return ctx
	}
	return context.WithValue(ctx, runLineageKey{}, lineage)
}

// RunLineageOf returns the lineage on ctx, nil when there is none. The caller must not change it.
func RunLineageOf(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	lineage, _ := ctx.Value(runLineageKey{}).([]string)
	return lineage
}

// lineageKeySeq numbers the lineage keys of the process (NewRunLineageKey).
var lineageKeySeq atomic.Uint64

// NewRunLineageKey returns a lineage key for one run, unique in the process: label, then "#" and a
// number no other key of the process has. A run id is not unique enough: each sub-agent manager
// draws its ids alone, so two managers can hold the same one, and a stop cascade keyed by the id
// would reach the other run's work. The key is held in memory only; the run's id, manifest,
// resume and output are unchanged.
func NewRunLineageKey(label string) string {
	return label + "#" + strconv.FormatUint(lineageKeySeq.Add(1), 10)
}

// ExtendRunLineage returns a new lineage of lineage followed by keys; lineage is not changed.
func ExtendRunLineage(lineage []string, keys ...string) []string {
	out := make([]string, 0, len(lineage)+len(keys))
	return append(append(out, lineage...), keys...)
}
