package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/serverconfig"
)

// runContentTruncationMarker ends the text a late subscriber is given when the in-memory
// fallback hit its cap, so the cut is visible. The committed message is not affected.
const runContentTruncationMarker = "\n\n[... output truncated: the server could not keep the full streamed text; it is complete once the run finishes ...]"

const runPartialOwnerSuffix = ".owner"

type partialOwner struct {
	Hostname string `json:"hostname"`
	PID      int    `json:"pid"`
}

// runContentCache stores the active assistant text. The current Go UI stream
// exposes output_text as one merged part, so appending in provider-event order
// reconstructs that part, including interleaved upstream text deltas. Small
// values stay in memory; larger values spill as raw UTF-8 bytes to one
// ephemeral partial file. The file is not transcript data and is removed at
// run retirement; no per-item range index is needed for this single-part path.
type runContentCache struct {
	mu          sync.Mutex
	memoryLimit int
	// fallbackMax caps memory once fallback is set: past it new text is dropped from the
	// cache (the live stream and the transcript still get it) and truncated is set.
	fallbackMax     int
	truncated       bool
	writeBatchBytes int
	flushInterval   time.Duration
	partialPath     string
	ownerPath       string
	memory          []byte
	pending         []byte
	file            *os.File
	fileBytes       int64
	timer           *time.Timer
	ownsFile        bool
	ownsOwner       bool
	fallback        bool
	closed          bool
	warned          bool
	// total is the bytes appended so far. It is written under mu after the bytes are
	// stored, so a reader that loads it knows that many bytes can be read back.
	total atomic.Int64
	// pins counts snapshot readers that need the data after Close; Close then only
	// marks the cache closed and the last unpin finishes it. closeRemove and
	// closeDeferred are guarded by mu.
	pins          atomic.Int32
	closeDeferred bool
	closeRemove   bool
	// logger receives the one-time degradation warning; nil discards it.
	logger *slog.Logger
}

func newRunContentCache(memoryLimit, writeBatchBytes int, flushInterval time.Duration) *runContentCache {
	return &runContentCache{
		memoryLimit:     memoryLimit,
		fallbackMax:     serverconfig.DefaultRunContentFallbackMaxBytes,
		writeBatchBytes: writeBatchBytes,
		flushInterval:   flushInterval,
	}
}

func (c *runContentCache) SetPartialPath(path string) {
	if c == nil || path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.partialPath = filepath.Clean(path)
	c.ownerPath = c.partialPath + runPartialOwnerSuffix
	if c.file == nil && !c.fallback && len(c.memory) > c.memoryLimit {
		c.spillLocked("")
	}
}

func (c *runContentCache) Append(delta string) {
	if c == nil || delta == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	defer c.recordAppendLocked(len(delta))
	if c.fallback {
		c.appendMemoryLocked(delta)
		return
	}
	if c.file == nil {
		if c.partialPath == "" || (c.memoryLimit > 0 && len(c.memory)+len(delta) <= c.memoryLimit) {
			c.memory = append(c.memory, delta...)
			return
		}
		c.spillLocked(delta)
		return
	}
	c.appendSpilledLocked(delta)
}

func (c *runContentCache) Snapshot() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.fallback || c.file == nil {
		return c.memoryTextLocked(len(c.memory))
	}
	if !c.flushPendingLocked() {
		return c.memoryTextLocked(len(c.memory))
	}
	content, err := c.readStableLocked()
	if err != nil {
		c.switchToFallbackLocked(nil, err)
		return c.memoryTextLocked(len(c.memory))
	}
	return string(content)
}

// memoryTextLocked is the first n bytes of memory, followed by the truncation marker
// when n reaches the end of a truncated cache.
func (c *runContentCache) memoryTextLocked(n int) string {
	atEnd := n >= len(c.memory)
	if atEnd {
		n = len(c.memory)
	}
	if atEnd && c.truncated {
		return string(c.memory[:n]) + runContentTruncationMarker
	}
	return string(c.memory[:n])
}

// recordAppendLocked keeps total equal to the bytes the cache can read back: in fallback
// that is what memory holds, which the cap may have cut below what was appended.
func (c *runContentCache) recordAppendLocked(appended int) {
	if c.fallback {
		c.total.Store(int64(len(c.memory)))
		return
	}
	c.total.Add(int64(appended))
}

// appendMemoryLocked adds text to the fallback copy up to fallbackMax, cutting at a UTF-8
// boundary. The first cut is logged; text after it is dropped.
func (c *runContentCache) appendMemoryLocked(text string) {
	if c.truncated {
		return
	}
	room := c.fallbackMax - len(c.memory)
	if len(text) <= room {
		c.memory = append(c.memory, text...)
		return
	}
	if room < 0 {
		room = 0
	}
	for room > 0 && !utf8.RuneStart(text[room]) {
		room--
	}
	c.memory = append(c.memory, text[:room]...)
	c.markTruncatedLocked()
}

func (c *runContentCache) markTruncatedLocked() {
	if c.truncated {
		return
	}
	c.truncated = true
	logging.Or(c.logger).Warn("run content cache truncated; late subscribers see a shortened text until the run finishes",
		slog.Int("limit_bytes", c.fallbackMax))
}

// Truncated reports whether the fallback cap cut the text this cache can return.
func (c *runContentCache) Truncated() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// Length is the bytes appended so far; SnapshotPrefix(Length()) reads them back.
func (c *runContentCache) Length() int {
	if c == nil {
		return 0
	}
	return int(c.total.Load())
}

// Pin keeps the content readable by SnapshotPrefix until Unpin: a Close in between is
// finished by the last Unpin. Pin must be called while the cache cannot yet have been
// closed (the caller holds the lock that orders it before Close).
func (c *runContentCache) Pin() {
	if c != nil {
		c.pins.Add(1)
	}
}

func (c *runContentCache) Unpin() {
	if c == nil || c.pins.Add(-1) != 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeDeferred && c.pins.Load() == 0 {
		c.closeDeferred = false
		c.finishCloseLocked(c.closeRemove)
	}
}

// SnapshotPrefix returns the first n bytes appended, the content as it was when
// Length returned n. The cache must be pinned.
func (c *runContentCache) SnapshotPrefix(n int) string {
	if c == nil || n <= 0 {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if !c.fallback && c.file != nil && c.flushPendingLocked() {
		if int64(n) > c.fileBytes {
			n = int(c.fileBytes)
		}
		content := make([]byte, n)
		read, err := c.file.ReadAt(content, 0)
		if err == nil || read == n {
			return string(content)
		}
		c.switchToFallbackLocked(nil, err)
	}
	return c.memoryTextLocked(n)
}

func (c *runContentCache) Close(removePartial bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pins.Load() > 0 {
		c.closed = true
		c.closeDeferred = true
		c.closeRemove = c.closeRemove || removePartial
		return
	}
	c.finishCloseLocked(removePartial)
}

func (c *runContentCache) finishCloseLocked(removePartial bool) {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			c.warnLocked("close partial content file: %v", err)
		}
		c.file = nil
	}
	c.closed = true
	if removePartial {
		c.removePartialLocked()
	}
}

func (c *runContentCache) spillLocked(firstDelta string) {
	if c.partialPath == "" {
		c.memory = append(c.memory, firstDelta...)
		return
	}
	if err := c.openPartialLocked(); err != nil {
		c.switchToFallbackLocked([]byte(firstDelta), err)
		return
	}
	prior := append([]byte(nil), c.memory...)
	if len(prior) > 0 {
		if err := c.writeBytesLocked(prior); err != nil {
			c.switchToFallbackLocked(append(prior, firstDelta...), err)
			return
		}
	}
	c.memory = nil
	if firstDelta != "" {
		c.appendSpilledLocked(firstDelta)
	}
}

func (c *runContentCache) openPartialLocked() error {
	if c.file != nil {
		return nil
	}
	if c.partialPath == "" {
		return errors.New("partial content path is unavailable")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get partial-content owner hostname: %w", err)
	}
	ownerBytes, err := json.Marshal(partialOwner{Hostname: hostname, PID: os.Getpid()})
	if err != nil {
		return fmt.Errorf("encode partial-content owner: %w", err)
	}
	owner, err := os.OpenFile(c.ownerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create partial-content owner: %w", err)
	}
	if _, err := owner.Write(append(ownerBytes, '\n')); err != nil {
		_ = owner.Close()
		_ = os.Remove(c.ownerPath)
		return fmt.Errorf("write partial-content owner: %w", err)
	}
	if err := owner.Close(); err != nil {
		_ = os.Remove(c.ownerPath)
		return fmt.Errorf("close partial-content owner: %w", err)
	}
	c.ownsOwner = true
	file, err := os.OpenFile(c.partialPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.Remove(c.ownerPath)
		c.ownsOwner = false
		return fmt.Errorf("create partial content file: %w", err)
	}
	c.file = file
	c.ownsFile = true
	return nil
}

func (c *runContentCache) appendSpilledLocked(text string) {
	for len(text) > 0 {
		room := c.writeBatchBytes - len(c.pending)
		if room <= 0 {
			if !c.flushPendingLocked() {
				c.appendMemoryLocked(text)
				return
			}
			room = c.writeBatchBytes
		}
		n := len(text)
		if n > room {
			n = room
		}
		c.pending = append(c.pending, text[:n]...)
		text = text[n:]
		if len(c.pending) >= c.writeBatchBytes || c.flushInterval <= 0 {
			if !c.flushPendingLocked() {
				c.appendMemoryLocked(text)
				return
			}
		} else {
			c.scheduleFlushLocked()
		}
	}
}

func (c *runContentCache) flushPendingLocked() bool {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if len(c.pending) == 0 {
		return true
	}
	pending := append([]byte(nil), c.pending...)
	c.pending = nil
	if err := c.writeBytesLocked(pending); err != nil {
		c.switchToFallbackLocked(pending, err)
		return false
	}
	return true
}

func (c *runContentCache) writeBytesLocked(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if c.file == nil {
		return errors.New("partial content file is not open")
	}
	start := c.fileBytes
	if _, err := c.file.Seek(start, 0); err != nil {
		return fmt.Errorf("seek partial content file: %w", err)
	}
	remaining := data
	for len(remaining) > 0 {
		n, err := c.file.Write(remaining)
		if n > 0 {
			c.fileBytes += int64(n)
			remaining = remaining[n:]
		}
		if err != nil {
			c.rollbackWriteLocked(start)
			return fmt.Errorf("write partial content file: %w", err)
		}
		if n == 0 {
			c.rollbackWriteLocked(start)
			return errors.New("write partial content file: zero-byte write")
		}
	}
	return nil
}

func (c *runContentCache) rollbackWriteLocked(start int64) {
	if c.file == nil {
		return
	}
	if err := c.file.Truncate(start); err != nil {
		c.warnLocked("truncate partial content after write failure: %v", err)
	}
	_, _ = c.file.Seek(start, 0)
	c.fileBytes = start
}

func (c *runContentCache) readStableLocked() ([]byte, error) {
	return c.readStablePrefixLocked(c.fileBytes)
}

// readStablePrefixLocked reads the first limit bytes of the stable partial content, or all of it
// when it is shorter.
func (c *runContentCache) readStablePrefixLocked(limit int64) ([]byte, error) {
	if c.file == nil {
		return append([]byte(nil), c.memory...), nil
	}
	if limit > c.fileBytes {
		limit = c.fileBytes
	}
	if limit == 0 {
		return []byte{}, nil
	}
	content := make([]byte, limit)
	n, err := c.file.ReadAt(content, 0)
	if err != nil && n != len(content) {
		return nil, fmt.Errorf("read partial content file: %w", err)
	}
	if n != len(content) {
		return nil, io.ErrUnexpectedEOF
	}
	return content, nil
}

func (c *runContentCache) switchToFallbackLocked(extra []byte, cause error) {
	// The fallback keeps at most fallbackMax bytes, so only that prefix of the file is read.
	base, readErr := c.readStablePrefixLocked(int64(c.fallbackMax))
	if readErr != nil {
		c.warnLocked("read partial content for fallback: %v", readErr)
		base = nil
	}
	c.memory = nil
	c.pending = nil
	c.fallback = true
	c.appendMemoryLocked(string(base))
	c.appendMemoryLocked(string(extra))
	c.total.Store(int64(len(c.memory)))
	c.closeFileLocked()
	c.removePartialLocked()
	c.warnLocked("partial content cache disabled; using memory fallback: %v", cause)
}

func (c *runContentCache) closeFileLocked() {
	if c.file == nil {
		return
	}
	if err := c.file.Close(); err != nil {
		c.warnLocked("close partial content file: %v", err)
	}
	c.file = nil
}

func (c *runContentCache) removePartialLocked() {
	paths := make([]string, 0, 2)
	if c.ownsFile && c.partialPath != "" {
		paths = append(paths, c.partialPath)
	}
	if c.ownsOwner && c.ownerPath != "" {
		paths = append(paths, c.ownerPath)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.warnLocked("remove partial content file: %v", err)
		}
	}
	c.ownsFile = false
	c.ownsOwner = false
}

func (c *runContentCache) scheduleFlushLocked() {
	if c.flushInterval <= 0 || c.timer != nil || c.closed {
		return
	}
	c.timer = time.AfterFunc(c.flushInterval, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.timer = nil
		if c.closed || c.fallback {
			return
		}
		_ = c.flushPendingLocked()
	})
}

func (c *runContentCache) warnLocked(format string, args ...any) {
	if c.warned {
		return
	}
	c.warned = true
	logging.Or(c.logger).Warn("run content cache degraded", slog.String("detail", fmt.Sprintf(format, args...)))
}
