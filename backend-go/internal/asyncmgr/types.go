package asyncmgr

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// State is the unified handle state shown by the async tool.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateStopped  State = "stopped"
	StateTimedOut State = "timed_out"
	StateLost     State = "lost"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateStopped, StateTimedOut, StateLost:
		return true
	}
	return false
}

// Kind names a driver: process, subagent, tool_smith, profile_message.
type Kind string

// ServerID names the server process that wrote a record, as the process and sub-agent
// manifests do, so a later server can tell a live peer from a dead one.
type ServerID struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start,omitempty"`
}

const (
	// ReasonRestarted is the reason of a handle found running after a restart.
	ReasonRestarted = "backend_restarted"
	// ReasonShutdown is the reason of a handle stopped by Close.
	ReasonShutdown = "server_shutdown"

	labelMaxBytes  = 80
	refMaxBytes    = 512
	codeMaxBytes   = 64
	reasonMaxBytes = 128
	recordVersion  = 1
)

// ConfigNoteMaxBytes bounds a config note from a resumed remote run; remote views use it too.
const ConfigNoteMaxBytes = 1024

var (
	ErrUnknownHandle = errors.New("unknown_handle")
	ErrClosed        = errors.New("async manager is closed")
	ErrDuplicate     = errors.New("handle id already registered")
	ErrInvalidID     = errors.New("invalid handle id")
	ErrInvalidCursor = errors.New("invalid cursor")
	ErrNoDriver      = errors.New("no driver registered for kind")
	ErrInvalidResult = errors.New("result state must be terminal")
	// ErrForeignHandle refuses a stop of a live handle that another running server owns:
	// only that server's drivers know the run, so this one cannot stop it.
	ErrForeignHandle = errors.New("handle runs in another backend server")

	// ErrCapacity matches every *CapacityError (errors.Is).
	ErrCapacity = errors.New("async_capacity_reached")

	idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// CapacityError says which live-handle cap refused a start.
type CapacityError struct {
	Scope string // "owner" or "total"
	Limit int64
	Live  int64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("async_capacity_reached: %d live handles (%s limit %d); wait for one or stop it with the async tool", e.Live, e.Scope, e.Limit)
}

func (e *CapacityError) Is(target error) bool { return target == ErrCapacity }

// Entry is the fixed-shape signal/status record. Field order is part of the contract;
// empty fields are omitted. It never carries a full result: Ref points at it.
type Entry struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	State   State  `json:"state"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Label   string `json:"label,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Cut says the summary was shortened, or a resume field was dropped, to fit the response budget.
	Cut bool `json:"cut,omitempty"`
	// Bytes is the size of the full result a driver saved in the file Ref names; the summary
	// may be shorter. RefTranscript is the transcript of the work, for a kind that has one.
	Bytes         int64  `json:"bytes,omitempty"`
	Ref           string `json:"ref,omitempty"`
	RefTranscript string `json:"ref_transcript,omitempty"`
	// ResumedFrom and ConfigNote are set for a remote run that resumed another, and only then.
	ResumedFrom string `json:"resumed_from,omitempty"`
	ConfigNote  string `json:"config_note,omitempty"`
	T           string `json:"t"`
}

// ListItem is a list entry: a summary only, never output.
type ListItem struct {
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	State  State  `json:"state"`
	OK     bool   `json:"ok"`
	Label  string `json:"label,omitempty"`
	Unread bool   `json:"unread,omitempty"`
	T      string `json:"t"`
}

// Counts is the cheap header of list: live handles, unread terminal ones, all retained.
type Counts struct {
	Active int `json:"active"`
	Unread int `json:"unread"`
	Total  int `json:"total"`
}

// Record is the small per-handle file <dir>/<id>.json. The full result stays in the
// driver's own files, named by Ref.
type Record struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	Owner   string `json:"owner"`
	State   State  `json:"state"`
	OK      bool   `json:"ok,omitempty"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Label   string `json:"label,omitempty"`
	Summary string `json:"summary,omitempty"`
	Cut     bool   `json:"cut,omitempty"`
	// SrcCut: the driver had already shortened Summary, so every view shows it as cut.
	SrcCut bool   `json:"src_cut,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// RefTranscript is the transcript of the work; see Entry.
	RefTranscript string `json:"ref_transcript,omitempty"`
	// ResumedFrom and ConfigNote are kept from the result for the entry; see Entry.
	ResumedFrom string `json:"resumed_from,omitempty"`
	ConfigNote  string `json:"config_note,omitempty"`
	// Detail is the longer result text a driver keeps in the record itself (up to
	// status_summary_max_bytes), shown by status when the driver has no live detail.
	Detail    string `json:"detail,omitempty"`
	DetailCut bool   `json:"detail_cut,omitempty"`
	// Trace points at where the work can be inspected later. It is never put in an Entry.
	Trace     string    `json:"trace,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Unread    bool      `json:"unread,omitempty"`
	Overflow  bool      `json:"overflow,omitempty"`
	Server    *ServerID `json:"server,omitempty"`
}

// Driver is what a kind provides. Stop must not block: it asks the work to end and the
// driver later calls Manager.Complete. The manager never calls a driver while holding
// its own lock, and a driver must not call the manager while holding its own.
type Driver interface {
	Kind() Kind
	Stop(id, reason string)
}

// Detailer is optional: a driver that can describe a live handle in more than a
// summary (status). The text is cut to status_summary_max_bytes.
type Detailer interface {
	Detail(id string, maxBytes int) string
}

// Registration starts tracking one handle.
type Registration struct {
	ID    string
	Owner string
	Kind  Kind
	Label string
	// Queued starts the handle in the queued state (counts as live); MarkRunning moves it.
	Queued bool
}

// Result is the terminal outcome a driver reports.
type Result struct {
	State   State
	OK      bool // only honoured for StateDone
	Code    string
	Reason  string
	Summary string
	// Cut says the driver already shortened Summary (for example to its tail).
	Cut bool
	// Bytes is the size of the full result saved in the file Ref names (0 when none).
	Bytes int64
	Ref   string
	// RefTranscript is the transcript of the work (a path), for a kind that has one.
	RefTranscript string
	// Detail is the full result text kept in the record, cut to status_summary_max_bytes.
	Detail string
	// ResumedFrom and ConfigNote tell a resumed remote run apart from a fresh one.
	ResumedFrom string
	ConfigNote  string
	// Trace is a driver reference for later inspection; it stays out of every entry.
	Trace string
}

func cutUTF8(s string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
