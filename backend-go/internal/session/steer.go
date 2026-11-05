package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"jad/backend-go/internal/transcript"
)

// ErrSteerClosed is returned by SteerQueue.Push once the run has finished taking messages.
var ErrSteerClosed = errors.New("the run is no longer taking messages")

// ErrSteerFull is returned by SteerQueue.Push when the queue holds its limit.
var ErrSteerFull = errors.New("the run already holds the most queued messages")

// SteerQueue holds the user messages sent to a run while it works. The run stores them
// as user input before its next provider round, so the model sees them without the run
// stopping; a message that arrives as the run would end makes it go one more round. It is
// safe for concurrent use: the HTTP route pushes, the run's goroutine drains.
type SteerQueue struct {
	mu      sync.Mutex
	pending []json.RawMessage
	limit   int
	closed  bool
}

// NewSteerQueue returns a queue that holds at most limit messages; a limit below one
// refuses every message.
func NewSteerQueue(limit int) *SteerQueue {
	return &SteerQueue{limit: limit}
}

// Push queues one run input (the shapes POST /run takes: a string or a list of items). It
// is normalized and validated here, so a bad message is refused to its sender and never
// reaches the run. It returns how many messages wait now.
func (q *SteerQueue) Push(input json.RawMessage) (int, error) {
	if q == nil {
		return 0, ErrSteerClosed
	}
	items, err := NormalizeInput(input)
	if err != nil {
		return 0, err
	}
	for index, item := range items {
		if err := transcript.ValidateInputItem(index, item); err != nil {
			return 0, err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, ErrSteerClosed
	}
	if len(q.pending) >= q.limit {
		return len(q.pending), ErrSteerFull
	}
	q.pending = append(q.pending, append(json.RawMessage(nil), input...))
	return len(q.pending), nil
}

// drain returns the queued messages and empties the queue.
func (q *SteerQueue) drain() []json.RawMessage {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	pending := q.pending
	q.pending = nil
	return pending
}

// closeIfEmpty closes the queue and reports true when nothing waits; with a message
// waiting it stays open and reports false, so the run goes on to take it.
func (q *SteerQueue) closeIfEmpty() bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) > 0 {
		return false
	}
	q.closed = true
	return true
}

// Close stops taking messages and returns those still waiting (a run that failed or was
// stopped never stored them).
func (q *SteerQueue) Close() []json.RawMessage {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	pending := q.pending
	q.pending = nil
	return pending
}

// storeSteerMessages appends the queued messages as user input, in arrival order, and
// tells OnSteer about each.
func (r *Runner) storeSteerMessages(options RunOptions) error {
	for _, input := range options.Steer.drain() {
		items, err := NormalizeInput(input)
		if err != nil {
			return fmt.Errorf("normalize a steering message: %w", err)
		}
		for index, item := range items {
			items[index], _ = options.ImageStore.Externalize(item)
		}
		if _, err := r.store.AppendInputItemsAt(items...); err != nil {
			return fmt.Errorf("persist a steering message: %w", err)
		}
		if options.OnSteer != nil {
			options.OnSteer(items)
		}
	}
	return nil
}
