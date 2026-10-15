package session

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// toolBatchPanic is a panic recovered on a batch worker. Its text names only the
// panic value, because it can reach a model (a sub-agent's failure reason); the stack
// is added to the server log by LogValue when the error is logged.
type toolBatchPanic struct {
	value any
	stack []byte
}

func (p *toolBatchPanic) Error() string { return fmt.Sprintf("tool batch call panicked: %v", p.value) }

func (p *toolBatchPanic) LogValue() slog.Value {
	return slog.GroupValue(slog.String("message", p.Error()), slog.String("stack", string(p.stack)))
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
	// A panic in execute or onFinished runs on this worker goroutine, where
	// nothing above can recover it; it would end the process. It becomes the
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
	worker := func() {
		defer wg.Done()
		for {
			call, ok := claim()
			if !ok {
				return
			}
			if err := runOne(call); err != nil {
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
