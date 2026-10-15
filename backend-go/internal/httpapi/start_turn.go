package httpapi

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// turnQueue hands out turns per key in arrival order. A run start and a compaction both
// wait for the transcript outside the session lock, and without an order a start that
// arrived later could take the transcript each time the file lock is released.
type turnQueue struct {
	mu     sync.Mutex
	queues map[string]*list.List
}

// enter waits for the turn of key. The returned leave gives the turn to the next
// arrival and is safe to call more than once. A cancelled ctx leaves the queue.
func (q *turnQueue) enter(ctx context.Context, key string) (leave func(), err error) {
	ready := make(chan struct{})
	q.mu.Lock()
	if q.queues == nil {
		q.queues = make(map[string]*list.List)
	}
	waiting := q.queues[key]
	if waiting == nil {
		waiting = list.New()
		q.queues[key] = waiting
	}
	element := waiting.PushBack(ready)
	if waiting.Front() == element {
		close(ready)
	}
	q.mu.Unlock()

	var once sync.Once
	leave = func() {
		once.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			wasFirst := waiting.Front() == element
			waiting.Remove(element)
			switch {
			case waiting.Len() == 0:
				if q.queues[key] == waiting {
					delete(q.queues, key)
				}
			case wasFirst:
				close(waiting.Front().Value.(chan struct{}))
			}
		})
	}
	select {
	case <-ready:
		return leave, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

func (q *turnQueue) len(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if waiting := q.queues[key]; waiting != nil {
		return waiting.Len()
	}
	return 0
}

// turnKey is the queue key of one transcript, spelled as lockSession spells the session,
// with a request that names no profile keyed as the session's default profile.
func (h *Handler) turnKey(reference, profile string) string {
	if references, ok := h.resolver.(SessionReferences); ok {
		if canonical, err := references.CanonicalSessionReference(reference); err == nil {
			reference = canonical
		}
	}
	if profiles, ok := h.resolver.(SessionProfiles); ok {
		if canonical, err := profiles.CanonicalSessionProfile(reference, profile); err == nil {
			profile = canonical
		}
	}
	return reference + "\x00" + profile
}

// acquireRun starts a run once the transcript is free. See tryStartRun for why the wait
// for the transcript happens outside the session lock.
func (h *Handler) acquireRun(ctx context.Context, reference, profile, model string) (startedRun, error) {
	leave, err := h.startTurns.enter(ctx, h.turnKey(reference, profile))
	if err != nil {
		return startedRun{}, err
	}
	defer leave()
	for {
		started, busy, err := h.tryStartRun(ctx, reference, profile, model)
		if err != nil || !busy {
			return started, err
		}
		release, err := h.lockSessionProcess(ctx, reference, profile, model)
		if err != nil {
			return startedRun{}, err
		}
		release()
	}
}

// lockSessionAndTranscript returns with the session lock and the transcript lock both
// held, waiting for the transcript outside the session lock. release frees both and gives
// the turn to the next arrival, so a start that came after waits for the whole operation.
func (h *Handler) lockSessionAndTranscript(ctx context.Context, reference, profile string) (release func(), err error) {
	leave, err := h.startTurns.enter(ctx, h.turnKey(reference, profile))
	if err != nil {
		return nil, err
	}
	for {
		unlock := h.lockSession(reference)
		processUnlock, err := h.tryLockSessionProcess(ctx, reference, profile, "")
		if err == nil {
			return func() {
				processUnlock()
				unlock()
				leave()
			}, nil
		}
		unlock()
		if !errors.Is(err, ErrSessionBusy) {
			leave()
			return nil, err
		}
		wait, err := h.lockSessionProcess(ctx, reference, profile, "")
		if err != nil {
			leave()
			return nil, err
		}
		wait()
	}
}
