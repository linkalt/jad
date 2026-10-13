package memorytrace

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/serverconfig"
)

const (
	tracePrefix                 = "[provider-memory] "
	enableEnvironmentVariable   = "JAD_PROVIDER_MEMORY_TRACE"
	intervalEnvironmentVariable = "JAD_PROVIDER_MEMORY_TRACE_INTERVAL_MS"
	maxSafeTraceIntervalMS      = (1 << 53) - 1
	maxTraceDurationMS          = (1<<63 - 1) / int64(time.Millisecond)
)

// Snapshot reports process memory. Go exposes heap and runtime-managed memory
// directly; external and array-buffer values are zero because Go has no
// equivalent accounting categories.
type Snapshot struct {
	RSSBytes          int64 `json:"rss_bytes"`
	HeapTotalBytes    int64 `json:"heap_total_bytes"`
	HeapUsedBytes     int64 `json:"heap_used_bytes"`
	ExternalBytes     int64 `json:"external_bytes"`
	ArrayBuffersBytes int64 `json:"array_buffers_bytes"`
}

// Options configures the optional process-wide provider-run sampler.
type Options struct {
	Enabled        bool
	SampleInterval time.Duration
	ReadMemory     func() Snapshot
	WriteLine      func(string)
	Now            func() time.Time
}

type activeRun struct {
	startedAt time.Time
	start     Snapshot
	peak      Snapshot
}

// Trace samples process memory while one or more provider runs are active.
// Callers share one Trace per server process and receive per-run handles.
type Trace struct {
	mu         sync.Mutex
	options    Options
	active     map[uint64]activeRun
	nextID     uint64
	generation uint64
	ticker     *time.Ticker
	stop       chan struct{}
}

// Handle records non-content phase metadata for one run.
type Handle struct {
	trace *Trace
	id    uint64
}

type traceRecord struct {
	Component  string         `json:"component"`
	Event      string         `json:"event"`
	Timestamp  string         `json:"timestamp"`
	PID        int            `json:"pid"`
	ActiveRuns int            `json:"active_runs"`
	RunID      uint64         `json:"run_id,omitempty"`
	RunIDs     []uint64       `json:"run_ids,omitempty"`
	Phase      string         `json:"phase,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	ElapsedMS  int64          `json:"elapsed_ms,omitempty"`
	Memory     *Snapshot      `json:"memory,omitempty"`
	Start      *Snapshot      `json:"start_memory,omitempty"`
	Peak       *Snapshot      `json:"peak_memory,omitempty"`
}

// New constructs a sampler. Disabled traces are cheap no-ops.
func New(options Options) *Trace {
	if options.SampleInterval <= 0 {
		options.SampleInterval = time.Duration(serverconfig.DefaultProviderMemoryTraceIntervalMS) * time.Millisecond
	}
	if options.ReadMemory == nil {
		options.ReadMemory = processMemory
	}
	if options.WriteLine == nil {
		options.WriteLine = func(line string) { _, _ = fmt.Println(line) }
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Trace{options: options, active: make(map[uint64]activeRun)}
}

// FromEnvironment creates the production sampler from the documented opt-in
// environment variables. Invalid intervals use the default rather than
// affecting run startup.
func FromEnvironment() *Trace {
	interval := time.Duration(serverconfig.DefaultProviderMemoryTraceIntervalMS) * time.Millisecond
	if raw := strings.TrimSpace(os.Getenv(intervalEnvironmentVariable)); raw != "" {
		if milliseconds, err := strconv.ParseInt(raw, 10, 64); err == nil && milliseconds > 0 && milliseconds <= maxSafeTraceIntervalMS && milliseconds <= maxTraceDurationMS {
			interval = time.Duration(milliseconds) * time.Millisecond
		}
	}
	return New(Options{
		Enabled:        os.Getenv(enableEnvironmentVariable) == "1",
		SampleInterval: interval,
	})
}

// BeginRun starts one tracked provider run and returns its phase recorder.
func (t *Trace) BeginRun() Handle {
	if t == nil || !t.options.Enabled {
		return Handle{}
	}
	memory := t.options.ReadMemory()
	now := t.options.Now()
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	t.active[id] = activeRun{startedAt: now, start: memory, peak: memory}
	record := t.recordLocked("start", id)
	record.Memory = &memory
	t.ensureSamplingLocked()
	t.mu.Unlock()
	t.write(record)
	return Handle{trace: t, id: id}
}

// SampleNow forces the same sample operation used by the interval timer.
// It is exported to support deterministic tests and diagnostics.
func (t *Trace) SampleNow() {
	if t == nil || !t.options.Enabled {
		return
	}
	memory := t.options.ReadMemory()
	t.mu.Lock()
	if len(t.active) == 0 {
		t.mu.Unlock()
		return
	}
	t.updatePeaksLocked(memory)
	record := t.recordLocked("sample", 0)
	record.RunIDs = t.activeIDsLocked()
	record.Memory = &memory
	t.mu.Unlock()
	t.write(record)
}

// Enabled reports whether this run is being sampled.
func (h Handle) Enabled() bool { return h.trace != nil && h.trace.options.Enabled }

// Mark records a phase and metadata only. Callers must never include prompt,
// response, or tool-output content in details.
func (h Handle) Mark(phase string, details map[string]any) {
	if h.trace == nil || phase == "" {
		return
	}
	memory := h.trace.options.ReadMemory()
	t := h.trace
	t.mu.Lock()
	if _, ok := t.active[h.id]; !ok {
		t.mu.Unlock()
		return
	}
	t.updatePeaksLocked(memory)
	record := t.recordLocked("phase", h.id)
	record.Phase = phase
	record.Details = cloneDetails(details)
	record.Memory = &memory
	t.mu.Unlock()
	t.write(record)
}

// Stop closes a run sample. Repeated calls are harmless.
func (h Handle) Stop() {
	if h.trace == nil {
		return
	}
	t := h.trace
	memory := t.options.ReadMemory()
	now := t.options.Now()
	t.mu.Lock()
	run, ok := t.active[h.id]
	if !ok {
		t.mu.Unlock()
		return
	}
	t.updatePeaksLocked(memory)
	delete(t.active, h.id)
	record := t.recordLocked("end", h.id)
	record.ElapsedMS = maxInt64(0, now.Sub(run.startedAt).Milliseconds())
	record.Memory = &memory
	record.Start = &run.start
	record.Peak = &run.peak
	if len(t.active) == 0 {
		t.stopSamplingLocked()
	}
	t.mu.Unlock()
	t.write(record)
}

func (t *Trace) ensureSamplingLocked() {
	if t.ticker != nil {
		return
	}
	t.generation++
	generation := t.generation
	t.ticker = time.NewTicker(t.options.SampleInterval)
	t.stop = make(chan struct{})
	ticker, stop := t.ticker, t.stop
	go func() {
		for {
			select {
			case <-ticker.C:
				t.sampleGeneration(generation)
			case <-stop:
				return
			}
		}
	}()
}

func (t *Trace) sampleGeneration(generation uint64) {
	memory := t.options.ReadMemory()
	t.mu.Lock()
	if generation != t.generation || len(t.active) == 0 {
		t.mu.Unlock()
		return
	}
	t.updatePeaksLocked(memory)
	record := t.recordLocked("sample", 0)
	record.RunIDs = t.activeIDsLocked()
	record.Memory = &memory
	t.mu.Unlock()
	t.write(record)
}

func (t *Trace) stopSamplingLocked() {
	if t.ticker == nil {
		return
	}
	t.ticker.Stop()
	close(t.stop)
	t.ticker = nil
	t.stop = nil
	t.generation++
}

func (t *Trace) updatePeaksLocked(memory Snapshot) {
	for id, run := range t.active {
		run.peak = maxSnapshot(run.peak, memory)
		t.active[id] = run
	}
}

func (t *Trace) activeIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(t.active))
	for id := range t.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (t *Trace) recordLocked(event string, id uint64) traceRecord {
	return traceRecord{
		Component:  "provider-memory",
		Event:      event,
		Timestamp:  t.options.Now().UTC().Format(time.RFC3339Nano),
		PID:        os.Getpid(),
		ActiveRuns: len(t.active),
		RunID:      id,
	}
}

func (t *Trace) write(record traceRecord) {
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	defer func() { _ = recover() }()
	t.options.WriteLine(tracePrefix + string(line))
}

func processMemory() Snapshot {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	rss := int64(stats.Sys)
	if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(statm))
		if len(fields) > 1 {
			if pages, err := strconv.ParseInt(fields[1], 10, 64); err == nil && pages >= 0 {
				rss = pages * int64(os.Getpagesize())
			}
		}
	}
	return Snapshot{
		RSSBytes:          rss,
		HeapTotalBytes:    int64(stats.HeapSys),
		HeapUsedBytes:     int64(stats.HeapAlloc),
		ExternalBytes:     0,
		ArrayBuffersBytes: 0,
	}
}

func maxSnapshot(left, right Snapshot) Snapshot {
	return Snapshot{
		RSSBytes:          maxInt64(left.RSSBytes, right.RSSBytes),
		HeapTotalBytes:    maxInt64(left.HeapTotalBytes, right.HeapTotalBytes),
		HeapUsedBytes:     maxInt64(left.HeapUsedBytes, right.HeapUsedBytes),
		ExternalBytes:     maxInt64(left.ExternalBytes, right.ExternalBytes),
		ArrayBuffersBytes: maxInt64(left.ArrayBuffersBytes, right.ArrayBuffersBytes),
	}
}

func cloneDetails(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	cloned := make(map[string]any, len(details))
	for key, value := range details {
		cloned[key] = value
	}
	return cloned
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
