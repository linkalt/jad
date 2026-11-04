package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessiongate"
	"jad/backend-go/internal/tools"
)

// RunSession is the small session boundary used by the HTTP adapter.
type RunSession interface {
	Run(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error)
}

// ResolvedSession identifies the canonical session/profile key used for run
// attachment and provides the already configured session runner.
type ResolvedSession struct {
	Reference string
	Profile   string
	Runner    RunSession
}

// SessionResolver maps a requested session/profile/model path to a session
// runner. ModelPath is empty when resolving an attach or stop request.
type SessionResolver interface {
	Resolve(context.Context, string, string, string) (ResolvedSession, error)
}

// SessionProcessLocker is an optional adapter for serializing mutations across
// backend processes. Its lock is held across a full run or maintenance action.
type SessionProcessLocker interface {
	LockSession(context.Context, string, string, string) (func(), error)
	LockSessionSettings(context.Context, string) (func(), error)
}

// ErrSessionBusy is what TryLockSession returns when another run holds the transcript.
var ErrSessionBusy = errors.New("session transcript is locked by another run")

// SessionProcessTryLocker adds a lock attempt that does not wait. A run start uses it so
// it never waits for the transcript while holding the in-process session lock.
type SessionProcessTryLocker interface {
	TryLockSession(context.Context, string, string, string) (func(), error)
}

// SessionReferences is an optional adapter that names a session the one way every
// spelling of it resolves to (with a single session root, "demo" and "local:demo" are the
// same session).
type SessionReferences interface {
	CanonicalSessionReference(reference string) (string, error)
}

// SessionProfiles is an optional adapter that names the profile a request resolves to: a
// request that names none uses the session's default, which is the same transcript as a
// request that names that default.
type SessionProfiles interface {
	CanonicalSessionProfile(reference, profile string) (string, error)
}

// SessionChildren is an optional adapter for work a session started that outlives
// the run: the session's async handles (processes and sub-agent runs). CancelSessionChildren
// asks all of it to stop and reports how many were still going; it must not block.
type SessionChildren interface {
	CancelSessionChildren(reference, profile string) int
	// ResetSessionChildren does the same for a reset and drops the session's unread
	// completion signals.
	ResetSessionChildren(reference, profile string) int
}

// SessionWriters is an optional adapter that counts the sub-agent runs of a session that can
// change files and are going as handles, which outlive the run that started them. A
// checkpoint restore is refused while there is one, because it would rewrite files under it.
type SessionWriters interface {
	BackgroundWriters(reference, profile string) int
}

// SnapshotOptions selects one page of transcript-backed session messages.
type SnapshotOptions struct {
	Before           *int64
	CursorGeneration string
	Turns            *int
}

// CatalogResponse is a JSON body and its validator for a read-only catalog route.
type CatalogResponse struct {
	Body        json.RawMessage
	ETag        string
	NotModified bool
}

// SessionRunState describes one retained run for the frontend.
type SessionRunState struct {
	TurnID    string `json:"turn_id"`
	StartedAt string `json:"started_at"`
	Status    string `json:"status"`
	Source    string `json:"source,omitempty"`
	Caller    string `json:"caller,omitempty"`
}

// SessionRunSummaries carries the stored-default run and all per-profile runs.
type SessionRunSummaries struct {
	Default   *SessionRunState
	ByProfile map[string]SessionRunState
}

// SessionRunProvider exposes run state without coupling the app resolver to the HTTP registry.
type SessionRunProvider interface {
	SessionRuns(reference, defaultProfile string) SessionRunSummaries
}

// SessionCatalog is an optional read-only extension implemented alongside the
// run resolver. Keeping it separate lets the HTTP handler remain testable with
// small run-only resolvers.
type SessionCatalog interface {
	ListSessions(context.Context, string, SessionRunProvider) (CatalogResponse, error)
	ReadSessionSnapshot(context.Context, string, string, SnapshotOptions, string, SessionRunProvider) (CatalogResponse, error)
	ReadSessionHistory(context.Context, string, string, string) (CatalogResponse, error)
	ReadSessionMessage(context.Context, string, string, string, *int64) (json.RawMessage, error)
}

// SessionToolLister returns the effective provider-neutral tool definitions for
// one resolved session/profile. It is separate from SessionResolver so small
// run-only adapters can remain independently testable.
type SessionToolLister interface {
	ListTools(context.Context, string, string) (SessionTools, error)
}

// SessionTools is the answer of the session tools route. Tools is what the profile's
// model sees. Hidden lists the registered tools the profile's tools.allow keeps from its
// model; a sub-agent can still use them. Hidden is empty, and left out of the JSON, for a
// profile without a tools block.
type SessionTools struct {
	Tools  []tools.Definition `json:"tools"`
	Hidden []tools.Definition `json:"hidden,omitempty"`
}

type SessionModelInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type SessionModelOption struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
}

type SessionModelDefaultUpdate struct {
	ActiveProfile string               `json:"active_profile"`
	Profiles      []string             `json:"profiles"`
	ModelDefault  string               `json:"model_default"`
	Models        []string             `json:"models"`
	ModelOptions  []SessionModelOption `json:"model_options,omitempty"`
	Model         SessionModelInfo     `json:"model"`
	Changed       bool                 `json:"changed"`
	// ProfileETag is the entity tag of the profile file after the update; a later update sends
	// it back as If-Match, so a change made meanwhile (another tab, an editor) is refused.
	ProfileETag string `json:"profile_etag,omitempty"`
}

type SessionFileListing struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// SessionSettings is an optional filesystem-backed extension for session
// profile/model changes and safe session-file suggestions.
type SessionSettings interface {
	// UpdateModelDefault's last argument is the request's If-Match, empty when it sent none.
	UpdateModelDefault(context.Context, string, string, string, string) (SessionModelDefaultUpdate, error)
	ListSessionFiles(context.Context, string, string) (SessionFileListing, error)
}

// SessionImage is one stored image file.
type SessionImage struct {
	ContentType string
	Data        []byte
}

// SessionImages is an optional extension that serves the files of a session's image_store.
// The name is the <sha256>.<ext> of a reference; it is never a path.
type SessionImages interface {
	ReadSessionImage(ctx context.Context, reference, profile, name string) (SessionImage, error)
}

// MessageImages is an optional extension that serves an image a tool result still holds
// inline in the transcript. It is addressed by message, call id and position, never by a path.
type MessageImages interface {
	ReadMessageImage(ctx context.Context, reference, profile, messageID, callID string, index int, cursor *int64) (SessionImage, error)
}

type CompactionResponse struct {
	Compacted      bool            `json:"compacted"`
	CompactedItems int             `json:"compacted_items"`
	RetainedItems  int             `json:"retained_items"`
	Usage          json.RawMessage `json:"usage,omitempty"`
}

type FrontCompactionResponse struct {
	DeleteMode  string `json:"delete_mode"`
	ArchiveName string `json:"archive_name,omitempty"`
}

// StateContextResponse reports the block the user trigger wrote, or would write when
// the request was a preview. Written is false when there was nothing to write.
type StateContextResponse struct {
	Written bool   `json:"written"`
	Preview bool   `json:"preview,omitempty"`
	Role    string `json:"role,omitempty"`
	Text    string `json:"text,omitempty"`
	Bytes   int    `json:"bytes,omitempty"`
}

type SessionMutation interface {
	Compact(context.Context) (CompactionResponse, error)
	Reset(context.Context) error
	FrontCompact(context.Context, string) (FrontCompactionResponse, error)
	// StateContext fires the user trigger with the prompt the user typed; with preview
	// it renders the block without storing it.
	StateContext(ctx context.Context, prompt string, preview bool) (StateContextResponse, error)
}

type ResolvedMutation struct {
	Reference string
	Profile   string
	Session   SessionMutation
}

type SessionMaintenance interface {
	ResolveMaintenance(context.Context, string, string) (ResolvedMutation, error)
}

// SessionGater is an optional adapter that shares the mutation gate and the session locks
// with the rest of the backend. Without it the handler keeps its own gate.
type SessionGater interface {
	SessionGate() *sessiongate.Gate
}

// ProfileCompact is an optional adapter that compacts one profile's transcript under the same
// gate and locks the native profile tool takes. The compact route and the tool both call it.
type ProfileCompact interface {
	CompactProfile(ctx context.Context, reference, profile string) (CompactionResponse, error)
}

// ProfileReset is an optional adapter that resets one profile's transcript. The reset route
// and the native profile tool both call it, so the refusals are the same for both.
type ProfileReset interface {
	ResetProfile(ctx context.Context, reference, profile string) error
}

// RootLibrary provides the optional, read-only global Markdown library.
type RootLibrary interface {
	List(context.Context) (contentlibrary.Catalog, error)
	Read(context.Context, contentlibrary.Kind, string) (contentlibrary.Asset, bool, error)
}
