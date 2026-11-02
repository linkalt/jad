package session

import (
	"log/slog"
	"runtime/debug"
	"sync"

	"jad/backend-go/internal/tools"
)

// toolBatchPanic is a panic recovered on a batch worker. Its text is generic,
// because it can reach a model (a sub-agent's failure reason); the panic value and
// the stack are added to the server log by LogValue when the error is logged.
type toolBatchPanic struct {
	value any
	stack []byte
}

func (p *toolBatchPanic) Error() string { return "tool call panicked" }

func (p *toolBatchPanic) LogValue() slog.Value {
	return slog.GroupValue(slog.String("message", p.Error()), slog.Any("value", p.value), slog.String("stack", string(p.stack)))
}

// toolBatchConcurrency is how many calls of one response may run at once. With
// "ordered" scheduling a response holding a call that can change state runs one
// call at a time in the model's order, so a later call sees what an earlier one
// did; a response of read-only calls still runs together, up to limit. Any other
// scheduling value runs up to limit calls together.
func toolBatchConcurrency[Call any](calls []Call, limit int, scheduling string, changesState func(Call) bool) int {
	if scheduling != "ordered" {
		return limit
	}
	for _, call := range calls {
		if changesState(call) {
			return 1
		}
	}
	return limit
}

// runToolBatch runs the calls of one provider response with bounded
// concurrency. Every call is a leaf: see runToolBatchLeaves.
func runToolBatch[Call, Output any](
	calls []Call,
	limit int,
	execute func(Call) (Output, error),
	onFinished func(Call, Output) error,
	shouldStop func() bool,
) error {
	return runToolBatchLeaves(nil, calls, limit, func(Call) bool { return true }, execute, onFinished, shouldStop)
}

// runToolBatchLeaves runs the calls of one provider response with bounded
// concurrency. A model that returns several function calls in one response
// expects them to run in parallel, and each finished call is reported through
// onFinished as soon as it settles, so a client sees every result when it
// exists instead of waiting for the slowest call of the batch.
//
// Every call runs on its own goroutine. A leaf call (leaf reports true) also holds
// one token of the process-wide leaf budget from the moment its goroutine starts
// until it settles. A call that is not a leaf holds no token while it waits.
//
// Deadlock argument: a token holder is a leaf, and a leaf never calls
// runToolBatch, so it waits on nothing that needs a token. Every call that holds a
// token therefore settles and gives the token back. A call that is not a leaf holds
// no token while it waits, so the leaves it starts can always get one. No cycle of
// waits through the budget is possible.
//
// Failure policy matches sequential execution: after the first returned error
// (from execute or from onFinished), or once shouldStop reports true, no
// further call is started. Calls already in flight are allowed to settle, and
// the first error is returned for the caller to act on after it has persisted
// every result that did complete.
//
// A limit below one runs the calls strictly one after another.
func runToolBatchLeaves[Call, Output any](
	budget *tools.LeafBudget,
	calls []Call,
	limit int,
	leaf func(Call) bool,
	execute func(Call) (Output, error),
	onFinished func(Call, Output) error,
	shouldStop func() bool,
) error {
	if limit < 1 {
		limit = 1
	}
	if shouldStop == nil {
		shouldStop = func() bool { return false }
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	// A panic in execute or onFinished runs on the goroutine that runs the call,
	// where nothing above can recover it; it would end the process. It becomes the
	// call's error instead.
	runOne := func(call Call) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = &toolBatchPanic{value: recovered, stack: debug.Stack()}
			}
		}()
		output, err := execute(call)
		if err == nil {
			err = onFinished(call, output)
		}
		return err
	}
	// slots bounds the calls of this batch in flight at once; the budget bounds the
	// leaves of every batch in the process.
	slots := make(chan struct{}, limit)
	for _, call := range calls {
		slots <- struct{}{}
		if failed() || shouldStop() {
			<-slots
			break
		}
		isLeaf := leaf(call)
		if isLeaf {
			budget.Acquire()
		}
		wg.Add(1)
		go func(call Call, isLeaf bool) {
			defer wg.Done()
			defer func() {
				if isLeaf {
					budget.Release()
				}
				<-slots
			}()
			if err := runOne(call); err != nil {
				fail(err)
			}
		}(call, isLeaf)
	}
	wg.Wait()
	return firstErr
}
