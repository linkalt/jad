package tools

// LeafBudgetSize is the number of leaf tool calls that may run at once in one process.
// It is a package constant, not a config key: a known deviation from the config rule,
// with a follow-up to move it to config (see DECISIONS.md). 256 is four full batches of
// the largest per-response limit (64), so a few runs overlap at full fan-out while the
// process stays bounded.
const LeafBudgetSize = 256

// LeafBudget is a counting semaphore of leaf calls. A process creates one and passes it
// to every run, so the bound holds across all runs and batches.
type LeafBudget struct {
	tokens chan struct{}
}

// NewLeafBudget returns a budget of size tokens.
func NewLeafBudget(size int) *LeafBudget {
	return &LeafBudget{tokens: make(chan struct{}, size)}
}

// Acquire takes one token, waiting while all of them are in use. The caller must hold
// the token until its leaf call settles, then give it back with Release. A leaf call
// never waits on another leaf: see session.runToolBatchLeaves. A nil budget takes no
// token, so a caller that builds its run options by hand runs without a bound.
func (b *LeafBudget) Acquire() {
	if b == nil {
		return
	}
	b.tokens <- struct{}{}
}

// Release gives back a token taken by Acquire.
func (b *LeafBudget) Release() {
	if b == nil {
		return
	}
	<-b.tokens
}
