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
	// edges counts, per session, the calls blocked right now on a message: profile from
	// cannot go on until profile to answers. Only a call that waits is an edge; a handle
	// nobody blocks on is not.
	edges map[waitEdge]int
}

type waitEdge struct{ from, to string }

func (p *profileMessagePlan) edge() waitEdge {
	return waitEdge{from: chainNode(p.callerRef, p.caller), to: chainNode(p.reference, p.target)}
}

// beginWait records that a call of the caller's current run is blocked on the target until
// the returned function is called (once is enough; more are ignored).
func (s *profileQueueSet) beginWait(e waitEdge) func() {
	s.mu.Lock()
	if s.edges == nil {
		s.edges = map[waitEdge]int{}
	}
	s.edges[e]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.edges[e]--; s.edges[e] <= 0 {
				delete(s.edges, e)
			}
			s.mu.Unlock()
		})
	}
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

// waitsForCaller reports that a blocking message of p would close a loop of profiles each
// blocked on the next: the target is blocked, directly or through others, on the caller's
// profile (as a call waiting for an answer, or as a message waiting for a busy profile).
// Such calls could only end by timeout. The caller holds s.mu.
func (s *profileQueueSet) waitsForCaller(p *profileMessagePlan) bool {
	target, caller := chainNode(p.reference, p.target), chainNode(p.callerRef, p.caller)
	seen := map[string]bool{target: true}
	pending := []string{target}
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for edge := range s.edges {
			if edge.from != node {
				continue
			}
			if edge.to == caller {
				return true
			}
			if !seen[edge.to] {
				seen[edge.to] = true
				pending = append(pending, edge.to)
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
		if !head.beginStart() {
			// It ended (stopped or expired) after the line was read: nothing to start.
			s.remove(key, head)
			continue
		}
		run, err := head.plan.startRun(head.startCtx, head, true)
		s.remove(key, head)
		if err != nil {
			head.endStart()
			head.startFailed(err)
			// Its start is over and no run holds it: its node leaves the tree (finish may have
			// seen the start still going and left it).
			head.releaseNode()
			continue
		}
		head.attach(run)
	}
}

// profileMessagePlan is a message that passed every check and has not started.
type profileMessagePlan struct {
	// model is the model ID the target runs the message with; empty is its default.
	model     string
	resolver  *Resolver
	starter   httpapi.ProfileRunStarter
	key       string
	reference string
	target    string
	caller    string
	callerRef string
	chain     []string
	message   string
	timeout   time.Duration
	queueWait time.Duration
	maxQueued int
	blocking  bool
	hops      int
	// parentNode is the caller's node in the run tree (CallerContext.RunNode), empty for a user's
	// run; maxPerRoot is the caller's max_runs_per_root and maxPerSession the target's own
	// max_runs_per_session, the session the run executes in. The message's run joins the tree
	// under them (reserveNode).
	parentNode                string
	maxPerRoot, maxPerSession int
	// depth is the target run's depth across both kinds of run: the caller's plus one.
	depth int
	// deny is the caller's tools.deny ceiling (CallerContext.Deny), which narrows the target's run.
	deny []string
	// modelKeys is the caller's model key variables (CallerContext.ModelKeyEnvs), which the
	// target's run hides beside its own (T-009).
	modelKeys []string
	// lineage is the caller's run lineage with the caller's session key added; each message adds
	// its own key (profileMessageRun.lineageKey) for the run it starts.
	lineage   []string
	beforeRun func(context.Context) error
	// confine narrows the target's run by the caller's sandbox (D20); nil for a caller
	// without one.
	confine func(httpapi.RunSession) (httpapi.RunSession, error)
	// targetMessages and callerMessages are the transcripts of the target and of the caller.
	targetMessages, callerMessages string
	// foreign is a target in another workspace: its transcript is shown to the caller by name,
	// not by path.
	foreign bool
}

// TranscriptLabels implements native.ProfileTranscriptLabels for a target of another
// workspace: the qualified session and profile in place of the file of the target workspace,
// which the caller has no business knowing (the caller's own file is shown as it is).
func (p *profileMessagePlan) TranscriptLabels() (target, caller string) {
	if !p.foreign {
		return p.targetMessages, p.callerMessages
	}
	return p.reference + "/" + p.target, p.callerMessages
}

// Transcripts implements native.ProfileMessageTranscripts.
func (p *profileMessagePlan) Transcripts() (target, caller string) {
	return p.targetMessages, p.callerMessages
}

// startRun starts the target run of the message run through Resolver.StartRun (R5b), waiting for
// a busy target when wait is set.
func (p *profileMessagePlan) startRun(ctx context.Context, run *profileMessageRun, wait bool) (*httpapi.ProfileRun, error) {
	handle, err := p.resolver.StartRun(ctx, RunSpec{Kind: RunPersistent, Profile: p.request(run, wait)})
	return handle.ProfileRun(), err
}

func (p *profileMessagePlan) request(run *profileMessageRun, wait bool) httpapi.ProfileRunRequest {
	return httpapi.ProfileRunRequest{
		Reference: p.reference,
		Profile:   p.target,
		Caller:    p.caller,
		Chain:     p.chain,
		Message:   p.message,
		Model:     p.model,
		Hops:      p.hops,
		Depth:     p.depth,
		RunNode:   run.node.ID(),
		Wait:      wait,
		BeforeRun: p.beforeRun,
		Confine:   targetRunner(p.confine, p.caller, p.message, run.setWorktree, native.ExtendRunLineage(p.lineage, run.lineageKey), p.deny, p.modelKeys),
		OnCommitted: func(event json.RawMessage) {
			if text := native.SubagentAnswerText(event); strings.TrimSpace(text) != "" {
				run.lastText.Store(text)
			}
		},
		// The run's Done closes before httpapi releases the target's transcript; the message
		// ends only once it is released, so a message sent right after it finds the target free.
		OnEnd: func(*httpapi.ProfileRun) { run.releaseOnce.Do(func() { close(run.released) }) },
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
	// A message that is not handed to the caller (refused, or a panic on the way) leaves no node.
	handed := false
	defer func() {
		if !handed {
			r.releaseNode()
		}
	}()
	set := &p.resolver.profileQueues
	if p.blocking {
		r.startEnd = set.beginWait(p.edge())
	}
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
		if p.blocking && set.waitsForCaller(p) {
			set.mu.Unlock()
			q.try.Unlock()
			set.release(p.key, q)
			r.discard()
			return nil, native.ErrProfileCycle
		}
		// The run caps come after the target's full line and the wait cycle, which a busy target
		// is known to have here; a queued message holds its place until it ends.
		if err := r.reserveNode(); err != nil {
			set.mu.Unlock()
			q.try.Unlock()
			set.release(p.key, q)
			r.discard()
			return nil, err
		}
		r.queued = true
		q.waiting = append(q.waiting, r)
		set.mu.Unlock()
		q.try.Unlock()
		set.release(p.key, q)
		r.beginQueued()
		handed = true
		return r, nil
	}
	// The target looks free: the run caps are checked before the start is tried. A wait cycle,
	// which only a target that is itself blocked (so busy) can close, keeps its precedence as it
	// had when the start found the target busy and the message could queue.
	if err := r.reserveNode(); err != nil {
		if p.maxQueued > 0 && p.blocking && set.waitsForCaller(p) {
			err = native.ErrProfileCycle
		}
		set.mu.Unlock()
		q.try.Unlock()
		set.release(p.key, q)
		r.discard()
		return nil, err
	}
	set.mu.Unlock()

	run, err := p.startRun(ctx, r, false)
	if errors.Is(err, httpapi.ErrSessionBusy) && p.maxQueued > 0 {
		set.mu.Lock()
		if p.blocking && set.waitsForCaller(p) {
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
		handed = true
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
	handed = true
	return r, nil
}

// reserveNode admits the message's run into the run tree below the caller's node (a root for a
// user's run), executed in the target's session, under the caller's root cap and the target's
// own session cap (native.ErrRunTreeLimit beyond them). The node is counted from here until the
// message ends and, when a target run started, that run has released the target's transcript
// (releaseNode, endNode); the caller holds the queue set's lock, and the tree's lock is a leaf.
func (r *profileMessageRun) reserveNode() error {
	p := r.plan
	node, err := p.resolver.runTree.Reserve(native.RunReservation{
		Parent: p.parentNode, Session: p.key, Kind: native.RunNodeProfileMessage, State: native.RunNodeQueued,
		MaxPerRoot: p.maxPerRoot, MaxPerSession: p.maxPerSession,
	})
	if err != nil {
		return err
	}
	r.node = node
	return nil
}

// releaseNode takes the message's run out of the run tree; more calls do nothing. It is for a
// message whose target run never started, which left no work behind.
func (r *profileMessageRun) releaseNode() {
	r.node.Release()
}

// endNode gives back the places of the message's run once its target run ended and released the
// transcript, and keeps the node as the parent of what that run left going until that is gone
// (Resolver.releaseNodeAfter, under the run's lineage key).
func (r *profileMessageRun) endNode() {
	if r.plan.resolver == nil {
		r.node.Release()
		return
	}
	r.plan.resolver.releaseNodeAfter(r.node, r.lineageKey)
}

// beginStart marks that the pump is starting the message's target run, so a finish meanwhile
// leaves its node to whoever ends the start (attach, or the pump on a failed start). It reports
// false for a message that has already ended, which is not started.
func (r *profileMessageRun) beginStart() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return false
	}
	r.starting = true
	return true
}

// endStart marks that a start of the pump ended without a run.
func (r *profileMessageRun) endStart() {
	r.mu.Lock()
	r.starting = false
	r.mu.Unlock()
}

// profileMessageRun is one message in flight: queued for a busy target, or running as the
// target's own run. It is the handle's work, so it outlives the call that sent it.
type profileMessageRun struct {
	plan *profileMessagePlan
	// lineageKey is the key of the run this message starts in the run lineage of the work that
	// run starts; a stop of the message stops that work (attach).
	lineageKey string

	ctx    context.Context // the message's whole life; its deadline is the call timeout
	cancel context.CancelFunc
	// startCtx bounds the wait for the target; the queue wait ending cancels it.
	startCtx    context.Context
	startCancel context.CancelFunc

	queued   bool
	startEnd func()
	startedC chan struct{}
	doneC    chan struct{}
	began    time.Time
	// released closes when the attached target run has ended and released the transcript.
	released    chan struct{}
	releaseOnce sync.Once

	lastText atomic.Value
	// worktree is the end of the target's worktree, when it ran in one; set by the target
	// run before it ends.
	worktree atomic.Pointer[native.DelegatedWorktreeResult]

	mu           sync.Mutex
	run          *httpapi.ProfileRun
	stopReason   string
	queueExpired bool
	finished     bool
	outcome      native.ProfileMessageOutcome
	queueTimer   *time.Timer
	// runEnded is set once the attached target run has ended (it starts nothing more), and
	// cascaded once the work under lineageKey was stopped. A stop that reached the message
	// cascades exactly once after runEnded, whenever it came (endRun, Stop).
	runEnded, cascaded bool
	// stopWatch unregisters the callback that reacts to the context ending while the message waits.
	stopWatch func() bool
	// starting is set while the pump starts the message's target run (beginStart): a finish then
	// leaves the node to the end of that start.
	starting bool
	// node is the message's run in the resolver's run tree (reserveNode), set before the message
	// is handed out and never changed; it is counted until the message ended and its target run,
	// if one started, released the transcript, and then stays an ended parent while work that run
	// left goes (endNode).
	node *native.RunNode
}

func newProfileMessageRun(plan *profileMessagePlan) *profileMessageRun {
	r := &profileMessageRun{plan: plan, startedC: make(chan struct{}), doneC: make(chan struct{}), released: make(chan struct{}), began: time.Now()}
	if plan.resolver != nil {
		r.lineageKey = fmt.Sprintf("profile_run_%d", plan.resolver.lineageSeq.Add(1))
	}
	if plan.timeout > 0 {
		r.ctx, r.cancel = context.WithTimeout(context.Background(), plan.timeout)
	} else {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	r.startCtx, r.startCancel = context.WithCancel(r.ctx)
	return r
}

// discard releases a message that never became visible to the caller. Start's own deferred
// release gives back its node.
func (r *profileMessageRun) discard() {
	r.EndStartWait()
	r.startCancel()
	r.cancel()
}

// beginQueued starts the watchers of a message that waits: the queue wait limit and the
// goroutine that reacts to a stop or the timeout.
func (r *profileMessageRun) beginQueued() {
	r.mu.Lock()
	if r.plan.queueWait > 0 {
		r.queueTimer = time.AfterFunc(r.plan.queueWait, r.expireQueue)
	}
	r.stopWatch = context.AfterFunc(r.ctx, r.onCtxDone)
	r.mu.Unlock()
}

// BeginWait records that one more call is blocked on this message until the returned
// function is called.
func (r *profileMessageRun) BeginWait() func() {
	return r.plan.resolver.profileQueues.beginWait(r.plan.edge())
}

// EndStartWait ends the wait the sending call itself held from the start.
func (r *profileMessageRun) EndStartWait() {
	if r.startEnd != nil {
		r.startEnd()
	}
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
//
// A stop is chosen while the message is live and can land after the target run ended (attach's
// watch has then looked for a stop already), even after the message finished: what that run left
// going is stopped all the same, once.
func (r *profileMessageRun) Stop(reason string) {
	r.mu.Lock()
	late := r.runEnded && !r.cascaded
	if late {
		r.cascaded = true
	}
	finished := r.finished
	if !finished && r.stopReason == "" {
		r.stopReason = reason
	}
	r.mu.Unlock()
	if !finished {
		r.cancel()
	}
	if late {
		r.cascade(reason)
	}
}

// endRun marks that the attached target run has ended, and reports whether the work under the
// message's key is stopped now and with which reason: when a stop reached the message before.
func (r *profileMessageRun) endRun() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runEnded = true
	if r.cascaded || r.stopReason == "" {
		return "", false
	}
	r.cascaded = true
	return r.stopReason, true
}

// cascade stops the work under the message's key in every manager (Resolver.cancelDescendants).
func (r *profileMessageRun) cascade(reason string) {
	if r.lineageKey != "" && r.plan.resolver != nil {
		r.plan.resolver.cancelDescendants(r.lineageKey, reason)
	}
}

// onCtxDone reacts to a stop or the timeout while the message waits for its target. Once the
// message is attached, attach watches the run instead.
func (r *profileMessageRun) onCtxDone() {
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
	r.starting = false
	if r.finished {
		// The message ended (stopped or expired in the line) while its start was on its way: the
		// run is stopped at once, and what it may have started already goes with it once it ended.
		// The node is counted until that run released the target's transcript.
		reason := r.stopReason
		r.cascaded = true
		r.mu.Unlock()
		run.Stop()
		if reason == "" {
			reason = "target_stopped"
		}
		go func() {
			<-run.Done()
			r.cascade(reason)
			<-r.released
			r.endNode()
		}()
		return
	}
	r.run = run
	if r.queueTimer != nil {
		r.queueTimer.Stop()
	}
	r.mu.Unlock()
	r.node.SetState(native.RunNodeRunning)
	close(r.startedC)
	if r.ctx.Err() != nil {
		run.Stop()
	}
	// This goroutine is the message's one watch once it is attached: it stops the target run
	// when the message's context ends, and records the outcome once the run has ended and
	// released the target's transcript (OnEnd, which follows Done).
	go func() {
		select {
		case <-run.Done():
		case <-r.ctx.Done():
			run.Stop()
			<-run.Done()
		}
		<-r.released
		// A stopped message stops what its run left going (handles, sub-agent runs, further
		// messages), now that the run has ended and starts nothing more; a stop that comes later
		// does it from Stop. A message that ended on its own or timed out, unasked, leaves that work
		// alone, as a user's run does.
		if reason, stopped := r.endRun(); stopped {
			r.cascade(reason)
		}
		r.finish(r.conclude(run))
		r.endNode()
	}()
}

// conclude turns the end of the target run into the message's outcome. Whatever final text
// the target left is kept, whether or not it finished.
func (r *profileMessageRun) conclude(run *httpapi.ProfileRun) native.ProfileMessageOutcome {
	outcome := native.ProfileMessageOutcome{Trace: r.plan.reference + "/" + r.plan.target + "#" + run.TurnID(), Worktree: r.worktree.Load()}
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

// setWorktree records how the target's worktree ended (profileDelegation.report).
func (r *profileMessageRun) setWorktree(result native.DelegatedWorktreeResult) {
	r.worktree.Store(&result)
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
	stopWatch := r.stopWatch
	// A message no target run holds, and whose start is not on its way, is over: its node goes
	// now. Otherwise the end of that run or start releases it (attach, pump).
	ended := r.run == nil && !r.starting
	r.mu.Unlock()
	if ended {
		r.releaseNode()
	}
	close(r.doneC)
	if stopWatch != nil {
		stopWatch()
	}
	r.startCancel()
	r.cancel()
}
