package tools

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
