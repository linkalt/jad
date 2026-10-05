package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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
	mu              sync.Mutex
	memoryLimit     int
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
}

func newRunContentCache(memoryLimit, writeBatchBytes int, flushInterval time.Duration) *runContentCache {
	return &runContentCache{
		memoryLimit:     memoryLimit,
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
	if c.fallback {
		c.memory = append(c.memory, delta...)
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
		return string(c.memory)
	}
	if !c.flushPendingLocked() {
		return string(c.memory)
	}
	content, err := c.readStableLocked()
	if err != nil {
		c.switchToFallbackLocked(nil, err)
		return string(c.memory)
	}
	return string(content)
}

func (c *runContentCache) Close(removePartial bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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
				c.memory = append(c.memory, text...)
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
				c.memory = append(c.memory, text...)
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
	if c.file == nil {
		return append([]byte(nil), c.memory...), nil
	}
	if c.fileBytes == 0 {
		return []byte{}, nil
	}
	content := make([]byte, c.fileBytes)
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
	base, readErr := c.readStableLocked()
	if readErr != nil {
		c.warnLocked("read partial content for fallback: %v", readErr)
		base = nil
	}
	c.memory = append(base, extra...)
	c.pending = nil
	c.fallback = true
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
	log.Printf(format, args...)
}
