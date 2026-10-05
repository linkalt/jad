package session

import "sync"

// runToolBatch runs the calls of one provider response with bounded
// concurrency. A model that returns several function calls in one response
// expects them to run in parallel, and each finished call is reported through
// onFinished as soon as it settles, so a client sees every result when it
// exists instead of waiting for the slowest call of the batch.
//
// Failure policy matches sequential execution: after the first returned error
// (from execute or from onFinished), or once shouldStop reports true, no
// further call is started. Calls already in flight are allowed to settle, and
// the first error is returned for the caller to act on after it has persisted
// every result that did complete.
//
// A limit below one runs the calls strictly one after another.
func runToolBatch[Call, Output any](
	calls []Call,
	limit int,
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
		next     int
		firstErr error
		wg       sync.WaitGroup
	)
	// claim hands out the next call, or reports that the worker should stop.
	claim := func() (Call, bool) {
		mu.Lock()
		defer mu.Unlock()
		var zero Call
		if firstErr != nil || next >= len(calls) || shouldStop() {
			return zero, false
		}
		call := calls[next]
		next++
		return call, true
	}
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	worker := func() {
		defer wg.Done()
		for {
			call, ok := claim()
			if !ok {
				return
			}
			output, err := execute(call)
			if err == nil {
				err = onFinished(call, output)
			}
			if err != nil {
				fail(err)
			}
		}
	}
	workers := min(limit, len(calls))
	wg.Add(workers)
	for range workers {
		go worker()
	}
	wg.Wait()
	return firstErr
}
