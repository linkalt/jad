package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/tools/native"
)

// profileQueueSet holds, per target profile, the messages waiting for it to be free. One
// mutex guards every queue's list: the lists are tiny and touched only on arrival, start
// and end.
type profileQueueSet struct {
	mu     sync.Mutex
	queues map[string]*targetQueue
}

// targetQueue is the first-come-first-served line for one target. While pumping is set,
// a goroutine owns starting the head, so a later arrival always joins the line instead of
// taking a momentarily free target. try serializes the non-waiting start attempts of one
// target, so the line is never judged while an arrival is half-way through its attempt.
type targetQueue struct {
	try     sync.Mutex
	waiting []*profileMessageRun
	pumping bool
	refs    int
}

func (s *profileQueueSet) acquire(key string) *targetQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queues == nil {
		s.queues = map[string]*targetQueue{}
	}
	q := s.queues[key]
	if q == nil {
		q = &targetQueue{}
		s.queues[key] = q
	}
	q.refs++
	return q
}

func (s *profileQueueSet) release(key string, q *targetQueue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q.refs--
	if q.refs == 0 && !q.pumping && len(q.waiting) == 0 && s.queues[key] == q {
		delete(s.queues, key)
	}
}

func (s *profileQueueSet) remove(key string, run *profileMessageRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.queues[key]; q != nil {
		for i, waiting := range q.waiting {
			if waiting == run {
				q.waiting = append(q.waiting[:i:i], q.waiting[i+1:]...)
				return
			}
		}
	}
}

// position is the 1-based place of run in its line and the line's length, zero when it is
// not waiting.
func (s *profileQueueSet) position(key string, run *profileMessageRun) (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[key]
	if q == nil {
		return 0, 0
	}
	for i, waiting := range q.waiting {
		if waiting == run {
			return i + 1, len(q.waiting)
		}
	}
	return 0, len(q.waiting)
}

// waitsForCaller reports that queueing p would close a loop of profiles each waiting for
// the next: the target already waits, directly or through others, for the caller's
// profile. Such messages could only end by timeout. The caller holds s.mu.
func (s *profileQueueSet) waitsForCaller(p *profileMessagePlan) bool {
	seen := map[string]bool{p.target: true}
	pending := []string{p.target}
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, q := range s.queues {
			for _, waiting := range q.waiting {
				if waiting.plan.reference != p.reference || waiting.plan.caller != node {
					continue
				}
				next := waiting.plan.target
				if next == p.caller {
					return true
				}
				if !seen[next] {
					seen[next] = true
					pending = append(pending, next)
				}
			}
		}
	}
	return false
}

// pump starts the head of the line each time the target is free, in arrival order, until
// the line is empty. Only the head waits for the transcript; the rest wait their turn here.
// It owns one reference of q, taken by whoever started it.
func (s *profileQueueSet) pump(key string, q *targetQueue) {
	defer s.release(key, q)
	for {
		s.mu.Lock()
		if len(q.waiting) == 0 {
			q.pumping = false
			s.mu.Unlock()
			return
		}
		head := q.waiting[0]
		s.mu.Unlock()
		run, err := head.plan.starter.StartProfileRun(head.startCtx, head.plan.request(head, true))
		s.remove(key, head)
		if err != nil {
			head.startFailed(err)
			continue
		}
		head.attach(run)
	}
}

// profileMessagePlan is a message that passed every check and has not started.
type profileMessagePlan struct {
	resolver  *Resolver
	starter   httpapi.ProfileRunStarter
	key       string
	reference string
	target    string
	caller    string
	chain     []string
	message   string
	timeout   time.Duration
	queueWait time.Duration
	maxQueued int
	beforeRun func(context.Context)
}

func (p *profileMessagePlan) request(run *profileMessageRun, wait bool) httpapi.ProfileRunRequest {
	return httpapi.ProfileRunRequest{
		Reference: p.reference,
		Profile:   p.target,
		Caller:    p.caller,
		Chain:     p.chain,
		Message:   p.message,
		Wait:      wait,
		BeforeRun: p.beforeRun,
		OnCommitted: func(event json.RawMessage) {
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				run.lastText.Store(text)
			}
		},
	}
}

func (p *profileMessagePlan) busy(queueFull int) error {
	heldBy, _ := p.starter.RunningProfileRunCaller(p.reference, p.target)
	return &native.ProfileBusyError{HeldBy: heldBy, QueueFull: queueFull}
}

// Start sends the message now when the target is free and nobody waits for it, otherwise
// puts it at the end of the target's line (or refuses it when the line is full). ctx bounds
// only this start: the message goes on after the call ends.
func (p *profileMessagePlan) Start(ctx context.Context) (native.ProfileMessageRun, error) {
	r := newProfileMessageRun(p)
	set := &p.resolver.profileQueues
	q := set.acquire(p.key)
	q.try.Lock()
	set.mu.Lock()
	if q.pumping {
		if len(q.waiting) >= p.maxQueued {
			set.mu.Unlock()
			q.try.Unlock()
			set.release(p.key, q)
			r.discard()
			return nil, p.busy(p.maxQueued)
		}
		if set.waitsForCaller(p) {
			set.mu.Unlock()
			q.try.Unlock()
			set.release(p.key, q)
			r.discard()
			return nil, native.ErrProfileCycle
		}
		r.queued = true
		q.waiting = append(q.waiting, r)
		set.mu.Unlock()
		q.try.Unlock()
		set.release(p.key, q)
		r.beginQueued()
		return r, nil
	}
	set.mu.Unlock()

	run, err := p.starter.StartProfileRun(ctx, p.request(r, false))
	if errors.Is(err, httpapi.ErrSessionBusy) && p.maxQueued > 0 {
		set.mu.Lock()
		if set.waitsForCaller(p) {
			set.mu.Unlock()
			q.try.Unlock()
			set.release(p.key, q)
			r.discard()
			return nil, native.ErrProfileCycle
		}
		r.queued = true
		q.pumping = true
		q.waiting = append(q.waiting, r)
		set.mu.Unlock()
		q.try.Unlock()
		r.beginQueued()
		go set.pump(p.key, q)
		return r, nil
	}
	q.try.Unlock()
	set.release(p.key, q)
	if err != nil {
		r.discard()
		if errors.Is(err, httpapi.ErrSessionBusy) {
			return nil, p.busy(0)
		}
		return nil, err
	}
	r.attach(run)
	return r, nil
}

// profileMessageRun is one message in flight: queued for a busy target, or running as the
// target's own run. It is the handle's work, so it outlives the call that sent it.
type profileMessageRun struct {
	plan *profileMessagePlan

	ctx    context.Context // the message's whole life; its deadline is the call timeout
	cancel context.CancelFunc
	// startCtx bounds the wait for the target; the queue wait ending cancels it.
	startCtx    context.Context
	startCancel context.CancelFunc

	queued   bool
	startedC chan struct{}
	doneC    chan struct{}
	began    time.Time

	lastText atomic.Value

	mu           sync.Mutex
	run          *httpapi.ProfileRun
	stopReason   string
	queueExpired bool
	finished     bool
	outcome      native.ProfileMessageOutcome
	queueTimer   *time.Timer
}

func newProfileMessageRun(plan *profileMessagePlan) *profileMessageRun {
	r := &profileMessageRun{plan: plan, startedC: make(chan struct{}), doneC: make(chan struct{}), began: time.Now()}
	if plan.timeout > 0 {
		r.ctx, r.cancel = context.WithTimeout(context.Background(), plan.timeout)
	} else {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	r.startCtx, r.startCancel = context.WithCancel(r.ctx)
	return r
}

// discard releases a message that never became visible to the caller.
func (r *profileMessageRun) discard() {
	r.startCancel()
	r.cancel()
}

// beginQueued starts the watchers of a message that waits: the queue wait limit and the
// goroutine that reacts to a stop or the timeout.
func (r *profileMessageRun) beginQueued() {
	if r.plan.queueWait > 0 {
		r.mu.Lock()
		r.queueTimer = time.AfterFunc(r.plan.queueWait, r.expireQueue)
		r.mu.Unlock()
	}
	go r.watch()
}

func (r *profileMessageRun) Queued() bool             { return r.queued }
func (r *profileMessageRun) Started() <-chan struct{} { return r.startedC }
func (r *profileMessageRun) Done() <-chan struct{}    { return r.doneC }

func (r *profileMessageRun) Outcome() native.ProfileMessageOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}

func (r *profileMessageRun) Status() string {
	r.mu.Lock()
	run := r.run
	r.mu.Unlock()
	if run != nil {
		return fmt.Sprintf("profile %s is working on the message: %ds used", r.plan.target, int64(time.Since(r.began).Seconds()))
	}
	position, length := r.plan.resolver.profileQueues.position(r.plan.key, r)
	if position == 0 {
		return fmt.Sprintf("profile %s is busy; the message is about to start", r.plan.target)
	}
	return fmt.Sprintf("profile %s is busy; the message waits its turn (%d of %d)", r.plan.target, position, length)
}

// Stop ends the message: a queued one leaves its line, a running one stops the target run.
// It does not wait.
func (r *profileMessageRun) Stop(reason string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	if r.stopReason == "" {
		r.stopReason = reason
	}
	r.mu.Unlock()
	r.cancel()
}

// watch reacts to a stop or the timeout while the message has not finished.
func (r *profileMessageRun) watch() {
	select {
	case <-r.doneC:
		return
	case <-r.ctx.Done():
	}
	select {
	case <-r.doneC:
		return
	default:
	}
	r.mu.Lock()
	run := r.run
	r.mu.Unlock()
	if run != nil {
		run.Stop()
		return
	}
	r.plan.resolver.profileQueues.remove(r.plan.key, r)
	r.finish(native.ProfileMessageOutcome{Err: r.endedError(), Text: r.partialText(), Reason: r.reason()})
}

// expireQueue ends a queued message whose wait for the target passed its limit.
func (r *profileMessageRun) expireQueue() {
	r.mu.Lock()
	if r.run != nil || r.finished {
		r.mu.Unlock()
		return
	}
	r.queueExpired = true
	r.mu.Unlock()
	r.startCancel()
	r.plan.resolver.profileQueues.remove(r.plan.key, r)
	r.finish(native.ProfileMessageOutcome{Err: r.endedError()})
}

func (r *profileMessageRun) reason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReason
}

func (r *profileMessageRun) partialText() string {
	text, _ := r.lastText.Load().(string)
	return text
}

// endedError is why a message that never ran, or whose run was cut, ended: the queue wait,
// the call timeout, or a stop.
func (r *profileMessageRun) endedError() error {
	r.mu.Lock()
	expired, reason := r.queueExpired, r.stopReason
	r.mu.Unlock()
	switch {
	case expired:
		return &native.ProfileBusyError{HeldBy: r.heldBy(), Waited: r.plan.queueWait}
	case reason == "" && errors.Is(r.ctx.Err(), context.DeadlineExceeded):
		return native.ErrProfileTimeout
	}
	return native.ErrProfileStopped
}

func (r *profileMessageRun) heldBy() string {
	heldBy, _ := r.plan.starter.RunningProfileRunCaller(r.plan.reference, r.plan.target)
	return heldBy
}

// startFailed ends a queued message whose start did not happen.
func (r *profileMessageRun) startFailed(err error) {
	if r.startCtx.Err() != nil {
		r.finish(native.ProfileMessageOutcome{Err: r.endedError(), Reason: r.reason()})
		return
	}
	if errors.Is(err, httpapi.ErrSessionBusy) {
		r.finish(native.ProfileMessageOutcome{Err: &native.ProfileBusyError{HeldBy: r.heldBy()}})
		return
	}
	r.finish(native.ProfileMessageOutcome{Err: err})
}

// attach binds the started target run and watches it to its end.
func (r *profileMessageRun) attach(run *httpapi.ProfileRun) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		run.Stop()
		return
	}
	r.run = run
	if r.queueTimer != nil {
		r.queueTimer.Stop()
	}
	r.mu.Unlock()
	close(r.startedC)
	if r.ctx.Err() != nil {
		run.Stop()
	}
	go func() {
		<-run.Done()
		r.finish(r.conclude(run))
	}()
	if !r.queued {
		go r.watch()
	}
}

// conclude turns the end of the target run into the message's outcome. Whatever final text
// the target left is kept, whether or not it finished.
func (r *profileMessageRun) conclude(run *httpapi.ProfileRun) native.ProfileMessageOutcome {
	outcome := native.ProfileMessageOutcome{Trace: r.plan.reference + "/" + r.plan.target + "#" + run.TurnID()}
	switch run.Status() {
	case "completed":
		answer := native.SubagentAnswerText(run.Final())
		if strings.TrimSpace(answer) == "" {
			answer = r.partialText()
		}
		if strings.TrimSpace(answer) == "" {
			outcome.Err = native.ErrProfileNoAnswer
			return outcome
		}
		outcome.Text = answer
	case "aborted":
		outcome.Text = r.partialText()
		outcome.Reason = r.reason()
		outcome.Err = r.endedError()
		if outcome.Reason == "" && r.ctx.Err() == nil {
			outcome.Reason = "target_stopped"
		}
	default:
		outcome.Text = r.partialText()
		outcome.Err = &native.ProfileFailedError{Message: run.Failure()}
	}
	return outcome
}

// finish records the outcome once and releases everything the message holds.
func (r *profileMessageRun) finish(outcome native.ProfileMessageOutcome) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.outcome = outcome
	if r.queueTimer != nil {
		r.queueTimer.Stop()
	}
	r.mu.Unlock()
	close(r.doneC)
	r.startCancel()
	r.cancel()
}
