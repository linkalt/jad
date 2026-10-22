// Package asyncmgr is the one manager behind every async native tool: it owns the
// handles, their persisted records and the queue of completion signals, and answers
// "wake me when any of them has something". It has no tool wiring; drivers (process,
// subagent, ...) register handles and report completions. See design_async_manager.md.
package asyncmgr

import "fmt"

// MaxSSEIDs is the largest sse_max_ids: the event is a hint, not a listing.
const MaxSSEIDs = 100

// Config is every number the manager uses. Each field is a tools.native.async key.
type Config struct {
	MaxHandles         int64 `json:"max_handles"`
	MaxHandlesTotal    int64 `json:"max_handles_total"`
	DefaultBlockMs     int64 `json:"default_block_ms"`
	MaxBlockMs         int64 `json:"max_block_ms"`
	DefaultWaitMs      int64 `json:"default_wait_ms"`
	MaxWaitMs          int64 `json:"max_wait_ms"`
	WaitMaxSignals     int64 `json:"wait_max_signals"`
	ListDefaultLimit   int64 `json:"list_default_limit"`
	ListMaxLimit       int64 `json:"list_max_limit"`
	SummaryMaxBytes    int64 `json:"summary_max_bytes"`
	StatusSummaryBytes int64 `json:"status_summary_max_bytes"`
	ResponseMaxBytes   int64 `json:"response_max_bytes"`
	// KeepFinished bounds the terminal records kept; RetentionDays prunes older ones
	// (0 = no age pruning).
	KeepFinished  int64 `json:"keep_finished"`
	RetentionDays int64 `json:"retention_days"`
	// UnreadMax bounds the undelivered signals per owner; UnreadTTLDays expires older
	// ones (0 = never).
	UnreadMax     int64 `json:"unread_max"`
	UnreadTTLDays int64 `json:"unread_ttl_days"`
	// StopWaitMs is how long a stop, and a shutdown, waits for a driver to report the
	// end of the handles it was told to stop (0 = do not wait).
	StopWaitMs int64 `json:"stop_wait_ms"`
	// SSEMaxIDs bounds the active handle ids a run stream's async_state event carries
	// (0 = counts only).
	SSEMaxIDs int64 `json:"sse_max_ids"`
	// IndexFlushMs delays index.json rewrites so a burst of state changes costs one
	// write (0 = write at every change). Record files are always written at once.
	IndexFlushMs int64 `json:"index_flush_ms"`
}

// MaxIndexFlushMs is the largest index_flush_ms.
const MaxIndexFlushMs = 60_000

// EntryOverheadBytes is the room one entry needs beside its summary: the fixed fields, a
// label, a reference and the JSON syntax. ResponseMaxBytes must hold one full entry.
const EntryOverheadBytes = 1024

// DefaultConfig returns the defaults of design_async_manager.md section 11.
func DefaultConfig() Config {
	return Config{
		MaxHandles: 16, MaxHandlesTotal: 64,
		DefaultBlockMs: 10_000, MaxBlockMs: 120_000,
		DefaultWaitMs: 10_000, MaxWaitMs: 120_000,
		WaitMaxSignals: 10, ListDefaultLimit: 10, ListMaxLimit: 50,
		SummaryMaxBytes: 512, StatusSummaryBytes: 4096, ResponseMaxBytes: 8192,
		KeepFinished: 200, RetentionDays: 14, UnreadMax: 100, UnreadTTLDays: 7,
		StopWaitMs: 2_000, SSEMaxIDs: 10, IndexFlushMs: 150,
	}
}

// Validate rejects a Config whose bounds contradict each other.
func (c Config) Validate() error {
	positive := []struct {
		name  string
		value int64
	}{
		{"max_handles", c.MaxHandles}, {"max_handles_total", c.MaxHandlesTotal},
		{"default_block_ms", c.DefaultBlockMs}, {"max_block_ms", c.MaxBlockMs},
		{"default_wait_ms", c.DefaultWaitMs}, {"max_wait_ms", c.MaxWaitMs},
		{"wait_max_signals", c.WaitMaxSignals}, {"list_default_limit", c.ListDefaultLimit},
		{"list_max_limit", c.ListMaxLimit}, {"summary_max_bytes", c.SummaryMaxBytes},
		{"status_summary_max_bytes", c.StatusSummaryBytes}, {"response_max_bytes", c.ResponseMaxBytes},
		{"keep_finished", c.KeepFinished}, {"unread_max", c.UnreadMax},
	}
	for _, field := range positive {
		if field.value < 1 {
			return fmt.Errorf("%s must be at least 1", field.name)
		}
	}
	if c.RetentionDays < 0 || c.UnreadTTLDays < 0 || c.StopWaitMs < 0 {
		return fmt.Errorf("retention_days, unread_ttl_days and stop_wait_ms must not be negative")
	}
	if c.IndexFlushMs < 0 || c.IndexFlushMs > MaxIndexFlushMs {
		return fmt.Errorf("index_flush_ms must be between 0 and %d", MaxIndexFlushMs)
	}
	if c.SSEMaxIDs < 0 || c.SSEMaxIDs > MaxSSEIDs {
		return fmt.Errorf("sse_max_ids must be between 0 and %d", MaxSSEIDs)
	}
	if c.MaxBlockMs < c.DefaultBlockMs {
		return fmt.Errorf("max_block_ms must be greater than or equal to default_block_ms")
	}
	if c.MaxWaitMs < c.DefaultWaitMs {
		return fmt.Errorf("max_wait_ms must be greater than or equal to default_wait_ms")
	}
	if c.ListMaxLimit < c.ListDefaultLimit {
		return fmt.Errorf("list_max_limit must be greater than or equal to list_default_limit")
	}
	if c.StatusSummaryBytes < c.SummaryMaxBytes {
		return fmt.Errorf("status_summary_max_bytes must be greater than or equal to summary_max_bytes")
	}
	if c.ResponseMaxBytes < c.SummaryMaxBytes+EntryOverheadBytes {
		return fmt.Errorf("response_max_bytes must be at least summary_max_bytes + %d so one full entry fits", EntryOverheadBytes)
	}
	if c.UnreadMax < c.WaitMaxSignals {
		return fmt.Errorf("unread_max must be greater than or equal to wait_max_signals")
	}
	return nil
}
