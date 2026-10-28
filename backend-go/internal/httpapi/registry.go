package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	interactionbroker "jad/backend-go/internal/interactions"
	"jad/backend-go/internal/serverconfig"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/webui"
)

type streamEvent struct {
	name  string
	frame []byte
}

type eventQueue struct {
	mu       sync.Mutex
	queue    []streamEvent
	bytes    int
	maxBytes int
	closed   bool
	// oversizedSnapshot is true while a full snapshot larger than the budget is
	// the frame at the front of the queue. That frame is the smallest unit the
	// protocol can send, so it is not charged against the frames behind it; it
	// leaves the budget when the client has read it (see next).
	oversizedSnapshot bool
	wake              chan struct{}

	// While holding, push parks events in held instead of queueing them: a late
	// subscriber is registered before its snapshot is built, and release puts the
	// snapshot first and then replays held in arrival order. holdSuperseded means a
	// full-state frame outgrew the hold budget and replaced what was held, so the
	// snapshot being built is older than a frame that follows it and is dropped.
	holding        bool
	held           []heldEvent
	heldBytes      int
	holdSuperseded bool
}

type heldEvent struct {
	event    streamEvent
	snapshot bool
}

func newEventQueue(maxBytes int) *eventQueue {
	return &eventQueue{maxBytes: maxBytes, wake: make(chan struct{}, 1)}
}

// terminalEvent reports whether name closes a run's stream: the answer, the
// failure and the end marker. They are finite and the connection ends right after
// them, so they are always queued, however large the message before them was.
func terminalEvent(name string) bool {
	return name == "completed" || name == "error" || name == "done"
}

func (q *eventQueue) push(event streamEvent, snapshot bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pushLocked(event, snapshot)
}

// startHold makes push park events until release. It is called before the queue is
// registered with a run, so no event can arrive earlier.
func (q *eventQueue) startHold() {
	q.mu.Lock()
	q.holding = true
	q.mu.Unlock()
}

// release ends the hold: lead is queued first (the snapshot, dropped when a newer
// full-state frame superseded it, then the small state frames of the same instant),
// then every held event in the order it arrived. Both happen in one critical section,
// so a concurrent push lands either in held or after the replay, never between.
func (q *eventQueue) release(snapshot *streamEvent, state []streamEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	held := q.held
	superseded := q.holdSuperseded
	q.held, q.heldBytes, q.holding, q.holdSuperseded = nil, 0, false, false
	if q.closed {
		return
	}
	if snapshot != nil && !superseded {
		q.pushLocked(*snapshot, true)
	}
	for _, event := range state {
		q.pushLocked(event, false)
	}
	for _, parked := range held {
		if !q.pushLocked(parked.event, parked.snapshot) {
			return
		}
	}
}

func (q *eventQueue) holdLocked(event streamEvent, snapshot bool) bool {
	size := len(event.frame)
	if size > q.maxBytes && snapshot {
		q.held, q.heldBytes, q.holdSuperseded = []heldEvent{{event, true}}, 0, true
		return true
	}
	if q.heldBytes+size > q.maxBytes && !(terminalEvent(event.name) && !snapshot) {
		if !snapshot {
			q.closeLocked()
			return false
		}
		q.held, q.heldBytes, q.holdSuperseded = nil, 0, true
	}
	q.held = append(q.held, heldEvent{event, snapshot})
	q.heldBytes += size
	return true
}

func (q *eventQueue) pushLocked(event streamEvent, snapshot bool) bool {
	if q.closed {
		return false
	}
	if q.holding {
		return q.holdLocked(event, snapshot)
	}
	if terminalEvent(event.name) {
		// A terminal snapshot replaces what is still queued only when it does not
		// fit behind it, since the client then needs just the final state; a
		// marker (done) always follows what is there.
		if snapshot && q.bytes+len(event.frame) > q.maxBytes {
			q.queue = nil
			q.bytes = 0
			q.oversizedSnapshot = false
		}
		q.queue = append(q.queue, event)
		q.bytes += len(event.frame)
		q.signalLocked()
		return true
	}

	if len(event.frame) > q.maxBytes {
		if !snapshot {
			q.closeLocked()
			return false
		}
		// It replaces everything queued and is not charged to the budget, so the
		// frames that follow it are bounded on their own.
		q.queue = []streamEvent{event}
		q.bytes = 0
		q.oversizedSnapshot = true
		q.signalLocked()
		return true
	}
	if snapshot && q.bytes+len(event.frame) > q.maxBytes {
		q.queue = nil
		q.bytes = 0
		q.oversizedSnapshot = false
	}
	if q.bytes+len(event.frame) > q.maxBytes {
		q.closeLocked()
		return false
	}
	q.queue = append(q.queue, event)
	q.bytes += len(event.frame)
	q.signalLocked()
	return true
}

func (q *eventQueue) next(ctx context.Context) (streamEvent, bool) {
	event, ok, _ := q.nextOrIdle(ctx, 0)
	return event, ok
}

// nextOrIdle is next with a quiet-stream signal: when idle is greater than zero
// and nothing arrives within it, it returns idle=true so the caller can write a
// keep-alive frame and wait again. The wait restarts after every event.
func (q *eventQueue) nextOrIdle(ctx context.Context, idle time.Duration) (event streamEvent, ok, timedOut bool) {
	var timer *time.Timer
	var idleC <-chan time.Time
	if idle > 0 {
		timer = time.NewTimer(idle)
		defer timer.Stop()
		idleC = timer.C
	}
	for {
		q.mu.Lock()
		if len(q.queue) > 0 {
			event := q.queue[0]
			q.queue[0] = streamEvent{}
			q.queue = q.queue[1:]
			if q.oversizedSnapshot {
				// The front frame was the uncharged oversized snapshot; the client
				// takes it now, so nothing is left to give back.
				q.oversizedSnapshot = false
			} else {
				q.bytes -= len(event.frame)
			}
			q.mu.Unlock()
			return event, true, false
		}
		if q.closed {
			q.mu.Unlock()
			return streamEvent{}, false, false
		}
		q.mu.Unlock()

		select {
		case <-q.wake:
		case <-idleC:
			return streamEvent{}, true, true
		case <-ctx.Done():
			return streamEvent{}, false, false
		}
	}
}

func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeLocked()
}

func (q *eventQueue) closeLocked() {
	q.closed = true
	q.queue = nil
	q.bytes = 0
	q.held, q.heldBytes = nil, 0
	q.signalLocked()
}

func (q *eventQueue) signalLocked() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

type runSummary = SessionRunState

type stopRunResult struct {
	Stopped bool   `json:"stopped"`
	TurnID  string `json:"turn_id,omitempty"`
}

type activeRun struct {
	registry  *runRegistry
	key       string
	turnID    string
	startedAt string
	source    string
	caller    string
	// chain is the profiles a tool-started run descends through, ending with its own; set
	// under mu before the run starts, empty for a user run.
	chain []string
	// hops is the remote hop count a tool-started run inherits; set under mu before it starts.
	hops                int
	cancel              context.CancelFunc
	ctx                 context.Context
	interactions        *interactionbroker.Broker
	supportsInteraction bool
	// onCommitted and onResult observe the run's committed responses and its final
	// response. They are set before the run starts and called on the run's goroutine.
	onCommitted func(event json.RawMessage)
	onResult    func(event json.RawMessage)

	mu                 sync.Mutex
	status             string
	pendingInteraction *tools.PendingInteraction
	interactionKnown   bool
	finishedAt         time.Time
	latest             *streamEvent
	latestMessage      *webUIMessage
	latestUsage        *streamEvent
	content            *runContentCache
	contentFinal       bool
	terminal           []streamEvent
	subscribers        map[*eventQueue]struct{}
	done               chan struct{}

	// interactionSeq is the newest broker transition applied per interaction id. Guarded by mu.
	interactionSeq map[string]uint64

	// providerRetry is the provider retry the run is waiting on, replayed to a client
	// that attaches mid-wait. nil when the run is not waiting. Guarded by mu.
	providerRetry *providerRetryWait

	// asyncState is the last async_state published, replayed to a client that attaches
	// later. nil until the session has handles to report. Guarded by mu.
	asyncState *AsyncState

	// liveImages holds the decoded inline images of the run's tool results by call id,
	// served by the live image route. Guarded by mu.
	liveImages map[string][]SessionImage
}

type runRegistry struct {
	mu        sync.Mutex
	runs      map[string][]*activeRun
	retention time.Duration
	maxQueue  int
	// maxRuns is the most runs that may be running at once; 0 means no cap.
	maxRuns int
	// running counts the runs whose status is "running": begin adds one under mu, finish
	// removes one when the run leaves that state, so the cap check never scans retained runs.
	running atomic.Int64
	closed  bool
}

func newRunRegistry(retention time.Duration, maxBufferedBytes, maxRuns int) *runRegistry {
	return &runRegistry{
		runs:      make(map[string][]*activeRun),
		retention: retention,
		maxQueue:  maxBufferedBytes,
		maxRuns:   maxRuns,
	}
}

func (r *runRegistry) begin(key, turnID string) (*activeRun, error) {
	return r.beginFrom(key, turnID, "", "")
}

func (r *runRegistry) beginFrom(key, turnID, source, caller string) (*activeRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("run registry is closed")
	}
	if source == "" && r.maxRuns > 0 && r.running.Load() >= int64(r.maxRuns) {
		return nil, &StatusError{
			Code:              http.StatusTooManyRequests,
			Message:           fmt.Sprintf("the server is already running %d runs (server.max_concurrent_runs); try again shortly", r.maxRuns),
			RetryAfterSeconds: serverconfig.RunCapRetryAfterSeconds,
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &activeRun{
		registry:    r,
		key:         key,
		turnID:      turnID,
		source:      source,
		caller:      caller,
		startedAt:   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		cancel:      cancel,
		ctx:         ctx,
		status:      "running",
		subscribers: make(map[*eventQueue]struct{}),
		done:        make(chan struct{}),
	}
	run.interactions = interactionbroker.NewBroker(run.setPendingInteraction)
	r.runs[key] = append(r.runs[key], run)
	if source == "" {
		r.running.Add(1)
	}
	return run, nil
}

func (r *runRegistry) current(key string) *activeRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.runs[key]
	for _, run := range entries {
		if run.state() == "running" {
			return run
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return entries[len(entries)-1]
}

func (r *runRegistry) SessionRuns(reference, defaultProfile string) SessionRunSummaries {
	if r == nil {
		return SessionRunSummaries{}
	}
	prefix := reference + "\x00"
	r.mu.Lock()
	selected := make(map[string]*activeRun)
	for key, entries := range r.runs {
		if !strings.HasPrefix(key, prefix) || len(entries) == 0 {
			continue
		}
		profile := strings.TrimPrefix(key, prefix)
		chosen := entries[len(entries)-1]
		for index := len(entries) - 1; index >= 0; index-- {
			if entries[index].state() == "running" {
				chosen = entries[index]
				break
			}
		}
		selected[profile] = chosen
	}
	r.mu.Unlock()

	summaries := SessionRunSummaries{}
	for profile, run := range selected {
		runSummary := run.summary()
		state := runSummary
		// The TypeScript registry reports a profile-less run only as the default
		// run; runs_by_profile lists runs of named profiles.
		if profile != "" {
			if summaries.ByProfile == nil {
				summaries.ByProfile = make(map[string]SessionRunState, len(selected))
			}
			summaries.ByProfile[profile] = state
		}
		if profile == defaultProfile {
			defaultRun := state
			summaries.Default = &defaultRun
		}
	}
	return summaries
}

func (r *runRegistry) stop(key string) stopRunResult {
	run := r.current(key)
	if run == nil {
		return stopRunResult{}
	}
	return run.stop()
}

// runningInSession returns the runs of every profile of a session that are still going.
func (r *runRegistry) runningInSession(reference string) []*activeRun {
	prefix := reference + "\x00"
	r.mu.Lock()
	var candidates []*activeRun
	for key, entries := range r.runs {
		if strings.HasPrefix(key, prefix) {
			candidates = append(candidates, entries...)
		}
	}
	r.mu.Unlock()
	running := candidates[:0]
	for _, run := range candidates {
		if run.state() == "running" {
			running = append(running, run)
		}
	}
	return running
}

func (r *runRegistry) close() {
	r.mu.Lock()
	r.closed = true
	var active []*activeRun
	for _, entries := range r.runs {
		active = append(active, entries...)
	}
	r.mu.Unlock()
	for _, run := range active {
		run.stop()
	}
}

func (r *runRegistry) retire(run *activeRun) {
	r.mu.Lock()
	entries := r.runs[run.key]
	for index, candidate := range entries {
		if candidate == run {
			entries = append(entries[:index], entries[index+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(r.runs, run.key)
	} else {
		r.runs[run.key] = entries
	}
	r.mu.Unlock()
	run.disposeContent()
}

func (r *activeRun) state() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *activeRun) summary() runSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runSummary{TurnID: r.turnID, StartedAt: r.startedAt, Status: r.status, Source: r.source, Caller: r.caller}
}

// subscribe attaches a client to the run's event stream.
//
// Invariant: the queue sees one gap-free, duplicate-free stream, in the order the run
// published it. For a running run the queue is registered, and the point P of the
// stream its snapshot describes is captured (latest message, content length, interaction,
// retry and usage state), in one critical section of r.mu. Every event published after P
// reaches the queue while it is holding, so it is parked in order; every event before P is
// part of the captured state. The snapshot (content read, marshal) is then built without
// r.mu, and release queues it first, then the small state frames, then the parked events,
// all under the queue's own lock.
func (r *activeRun) subscribe() *eventQueue {
	pending := r.beginSubscribe()
	pending.finish()
	return pending.queue
}

// pendingSubscribe is a subscriber registered at a point P whose snapshot is not built yet.
type pendingSubscribe struct {
	run      *activeRun
	queue    *eventQueue
	running  bool
	message  *webUIMessage
	fallback *streamEvent
	cache    *runContentCache
	length   int
	state    []streamEvent
}

func (r *activeRun) beginSubscribe() *pendingSubscribe {
	queue := newEventQueue(r.registry.maxQueue)
	pending := &pendingSubscribe{run: r, queue: queue}
	r.mu.Lock()
	defer r.mu.Unlock()

	summary, _ := makeSSEEvent("run", runSummary{TurnID: r.turnID, StartedAt: r.startedAt, Status: r.status, Source: r.source, Caller: r.caller})
	queue.push(summary, false)
	if r.status != "running" {
		if r.latestUsage != nil {
			queue.push(*r.latestUsage, false)
		}
		for _, event := range r.terminal {
			queue.push(event, event.name == "ui.message" || event.name == "completed")
		}
		return pending
	}

	pending.running = true
	queue.startHold()
	r.subscribers[queue] = struct{}{}
	if r.latestMessage != nil {
		message := *r.latestMessage
		message.Parts = append([]webui.Part(nil), message.Parts...)
		pending.message = &message
		if !r.contentFinal && r.content != nil && message.Text == "" {
			pending.cache = r.content
			pending.cache.Pin()
			pending.length = pending.cache.Length()
		}
	}
	pending.fallback = r.latest
	if r.interactionKnown {
		var interaction any
		if r.pendingInteraction != nil {
			copy := r.pendingInteraction.Clone()
			interaction = &copy
		}
		if event, err := makeSSEEvent("interaction", map[string]any{"turn_id": r.turnID, "pending": interaction}); err == nil {
			pending.state = append(pending.state, event)
		}
	}
	if r.providerRetry != nil {
		if event, err := makeSSEEvent("provider_retry", map[string]any{"turn_id": r.turnID, "waiting": r.providerRetry}); err == nil {
			pending.state = append(pending.state, event)
		}
	}
	if r.asyncState != nil {
		if event, err := makeSSEEvent("async_state", asyncStatePayload(r.turnID, *r.asyncState)); err == nil {
			pending.state = append(pending.state, event)
		}
	}
	if r.latestUsage != nil {
		pending.state = append(pending.state, *r.latestUsage)
	}
	return pending
}

// finish builds the snapshot outside r.mu and releases the held events behind it.
func (p *pendingSubscribe) finish() {
	if !p.running {
		return
	}
	defer p.cache.Unpin()
	var snapshot *streamEvent
	if p.message != nil {
		message := *p.message
		if p.cache != nil {
			injectContent(&message, p.cache.SnapshotPrefix(p.length))
		}
		if event, err := makeSSEEvent("ui.message", map[string]any{"message": message}); err == nil {
			snapshot = &event
		}
	}
	if snapshot == nil {
		snapshot = p.fallback
	}
	p.queue.release(snapshot, p.state)
}

func (r *activeRun) detach(queue *eventQueue) {
	r.mu.Lock()
	delete(r.subscribers, queue)
	r.mu.Unlock()
	queue.close()
}

func (r *activeRun) publishMessage(message webUIMessage) error {
	// Encoding a large message takes milliseconds and only needs the previous one, so
	// it runs outside the lock. If another writer replaced the previous message in
	// between, the frames are built again under the lock against the current one.
	r.mu.Lock()
	if r.status != "running" {
		r.mu.Unlock()
		return errors.New("run is not active")
	}
	previous := r.latestMessage
	r.mu.Unlock()
	frames, err := messageFrames(previous, message)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return errors.New("run is not active")
	}
	if r.latestMessage != previous {
		if frames, err = messageFrames(r.latestMessage, message); err != nil {
			return err
		}
	}
	latestMessage := message
	r.latestMessage = &latestMessage
	for _, event := range frames {
		for queue := range r.subscribers {
			if !queue.push(event, event.name == "ui.message") {
				delete(r.subscribers, queue)
			}
		}
	}
	return nil
}

// messageFrames returns the SSE frames that take a client from previous to message
// (the whole message when there is no previous one).
func messageFrames(previous *webUIMessage, message webUIMessage) ([]streamEvent, error) {
	var updates []webui.StreamEvent
	if previous == nil {
		updates = []webui.StreamEvent{{Name: "ui.message", Data: map[string]any{"message": message}}}
	} else {
		updates = webui.Increments(*previous, message)
	}
	frames := make([]streamEvent, 0, len(updates))
	for _, update := range updates {
		event, err := makeSSEEvent(update.Name, update.Data)
		if err != nil {
			return nil, err
		}
		frames = append(frames, event)
	}
	return frames, nil
}

func (r *activeRun) setContentCache(cache *runContentCache) {
	r.mu.Lock()
	r.content = cache
	r.mu.Unlock()
}

func (r *activeRun) setPartialContentPath(path string) {
	r.mu.Lock()
	cache := r.content
	r.mu.Unlock()
	if cache != nil {
		cache.SetPartialPath(path)
	}
}

func (r *activeRun) cacheContent(delta string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	cache := r.content
	r.mu.Unlock()
	if cache != nil {
		cache.Append(delta)
	}
}

func (r *activeRun) currentMessage() *webUIMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latestMessage == nil {
		return nil
	}
	message := r.snapshotMessageLocked()
	return &message
}

func (r *activeRun) snapshotMessageLocked() webUIMessage {
	message := *r.latestMessage
	message.Parts = append([]webui.Part(nil), message.Parts...)
	if !r.contentFinal && r.content != nil && message.Text == "" {
		injectContent(&message, r.content.Snapshot())
	}
	return message
}

// injectContent puts the run's streamed text into message (whose Parts it owns).
func injectContent(message *webUIMessage, text string) {
	message.Text = text
	if text == "" {
		return
	}
	textPart := -1
	for index := range message.Parts {
		if message.Parts[index].Type == "text" {
			textPart = index
			break
		}
	}
	if textPart < 0 {
		message.Parts = append(message.Parts, webui.Part{Type: "text"})
		textPart = len(message.Parts) - 1
	}
	message.Parts[textPart].Text = text
}

func (r *activeRun) setCommittedMessage(message webUIMessage) {
	r.mu.Lock()
	if r.status != "running" {
		r.mu.Unlock()
		return
	}
	message.Parts = append([]webui.Part(nil), message.Parts...)
	// A truncated cache holds only the start of the text plus a marker; the committed
	// message must never take that in place of the real text.
	if message.Text == "" && r.content != nil && !r.content.Truncated() {
		if text := r.content.Snapshot(); text != "" {
			message.Text = text
			if len(message.Parts) == 0 {
				message.Parts = []webui.Part{{Type: "text", Text: text}}
			} else if message.Parts[0].Type == "text" {
				message.Parts[0].Text = text
			}
		}
	}
	r.latestMessage = &message
	r.contentFinal = true
	cache := r.content
	r.mu.Unlock()
	if cache != nil {
		cache.Close(true)
	}
}

func (r *activeRun) disposeContent() {
	r.mu.Lock()
	cache := r.content
	r.content = nil
	r.liveImages = nil
	r.mu.Unlock()
	if cache != nil {
		cache.Close(true)
	}
}

func (r *activeRun) publish(name string, data any) error {
	event, err := makeSSEEvent(name, data)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if name == "ui.message" {
		r.latest = &event
	}
	if name == "usage" {
		r.latestUsage = &event
	}
	for queue := range r.subscribers {
		if !queue.push(event, name == "ui.message") {
			delete(r.subscribers, queue)
		}
	}
	r.mu.Unlock()
	return nil
}

func (r *activeRun) finish(status string, terminal []streamEvent) {
	if r.interactions != nil {
		r.interactions.Cancel("The run ended before the draft was answered.")
	}
	r.mu.Lock()
	stuck := r.pendingInteraction
	r.mu.Unlock()
	if stuck != nil {
		r.setPendingInteraction(nil, &tools.InteractionEnd{InteractionID: stuck.ID, Tool: stuck.Tool, Outcome: tools.InteractionCancelled})
	}
	r.mu.Lock()
	if r.status != "running" {
		r.mu.Unlock()
		return
	}
	r.status = status
	if r.source == "" {
		r.registry.running.Add(-1)
	}
	r.providerRetry = nil
	r.finishedAt = time.Now()
	r.terminal = append([]streamEvent(nil), terminal...)
	for _, event := range terminal {
		if event.name == "ui.message" {
			r.latest = &event
		}
		if event.name == "usage" {
			r.latestUsage = &event
		}
		for queue := range r.subscribers {
			if !queue.push(event, event.name == "ui.message" || event.name == "completed") {
				delete(r.subscribers, queue)
			}
		}
	}
	close(r.done)
	r.mu.Unlock()

	if r.registry.retention <= 0 {
		r.registry.retire(r)
		return
	}
	time.AfterFunc(r.registry.retention, func() { r.registry.retire(r) })
}

func (r *activeRun) stop() stopRunResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return stopRunResult{}
	}
	r.cancel()
	return stopRunResult{Stopped: true, TurnID: r.turnID}
}

func (r *activeRun) context() context.Context {
	return r.ctx
}

func makeSSEEvent(name string, data any) (streamEvent, error) {
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return streamEvent{}, errors.New("invalid SSE event name")
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return streamEvent{}, fmt.Errorf("encode SSE event data: %w", err)
	}
	frame := make([]byte, 0, len(name)+len(payload)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return streamEvent{name: name, frame: frame}, nil
}
