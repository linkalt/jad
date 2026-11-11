package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/httpdebug"
	"jad/backend-go/internal/inprocess"
	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/serverconfig"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessiongate"
	"jad/backend-go/internal/sessionlock"
	"jad/backend-go/internal/tools"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

// CatalogConfig holds the limits the root catalog and transcript views read.
type CatalogConfig struct {
	SnapshotTurns      int
	MaxPageTurns       int
	MaxInlinePartBytes int
	// StoreIdle is server.session_store_idle_ms: how long a session's transcript store
	// stays cached after its last use ends. Zero keeps every store until the session is
	// deleted or the server stops.
	StoreIdle time.Duration
}

// profileTurnsEntry is a profile's turn count with the file stamp of the transcript it was counted from.
type profileTurnsEntry struct {
	stamp string
	turns int64
}

// Resolver composes the root catalog, on-disk session configuration, transcript
// stores, and provider clients for the HTTP run routes.
type Resolver struct {
	// remoteRuns is the index of the remote run ids this server minted; see remoteRunIndex.
	remoteRuns *remoteRunIndex
	// profileQueues holds the profile_message calls waiting for a busy target profile.
	profileQueues profileQueueSet
	// runTree is the one run tree of the process (T-007): every sub-agent manager admits into it
	// (finishSubagentBuild) and every local profile_message run joins it (profileMessagePlan.Start),
	// so max_runs_per_root and max_runs_per_session count both kinds of run across managers.
	runTree native.RunTree
	// lineageSeq hands out the lineage keys of the runs profile_message starts (profileMessageRun).
	lineageSeq atomic.Uint64
	// runStops are the persistent runs going now, for the stop that reaches one while a call of
	// it can still start work (liveRunStops).
	runStops liveRunStops
	// runnerReturned, when set (a test), is called with the run context of each persistent run
	// once its runner returned, before the run ends (endRunStop).
	runnerReturned func(context.Context)
	// runMCPSeq numbers the managers of isolated runs' own MCP servers (runMCPLoader).
	runMCPSeq atomic.Uint64
	// compactProfile replaces compactTarget in tests.
	compactProfile func(ctx context.Context, reference, profile string) error
	// resetProfile replaces resetTarget in tests.
	resetProfile func(ctx context.Context, reference, profile string) error
	// profileUsage replaces profileMessenger.transcriptTokens in tests.
	profileUsage func(ctx context.Context, session, profile string) (*int64, error)
	logger       *slog.Logger
	skipped      skippedSessions
	roots        []sessionfs.Root
	environment  map[string]string
	catalog      CatalogConfig

	mu                sync.Mutex
	settingsMu        sync.Mutex
	lockedConfigs     lockedConfigs
	listSettingsCache listSettingsCache
	stores            map[string]*storeEntry
	profileTurnsMu    sync.Mutex
	profileTurnCounts map[string]profileTurnsEntry
	subagentManagers  map[string]*native.SubagentManager
	// budgetLedgers holds one session budget ledger per ledger path, so every profile of
	// a session counts into one; guarded by mu.
	budgetLedgers map[string]*session.BudgetLedger
	// leafBudget is the process-wide budget of leaf tool calls, shared by every run this
	// resolver starts, so the bound holds across sessions and sub-agents.
	leafBudget *tools.LeafBudget
	// asyncHosts holds one async manager per tool storage folder (its .async folder).
	asyncHosts    map[string]*native.AsyncHost
	asyncBuilds   map[string]chan struct{}
	openAsyncHost func(string, asyncmgr.Config, *slog.Logger) (*native.AsyncHost, error)
	mcpManagers   map[string]*mcpclient.Manager
	// mcpScopeRefs counts the holds of each manager of a narrowing (mcpScopeHold); guarded by mu.
	mcpScopeRefs map[*mcpclient.Manager]int
	// mcpDrains are the holds waiting for their run's lineage to drain (releaseMCPScopeAfter).
	mcpDrains mcpScopeDrains
	// nodeDrains are the ended message runs' tree nodes waiting for their lineage to drain
	// (releaseNodeAfter).
	nodeDrains    runNodeDrains
	taskStores    map[string]*native.TaskStore
	recordsStores map[string]*native.RecordsStore
	// storeSessions lists the session files that use each task and records store,
	// keyed by kind and storage key, so deleting the last of them can release the store.
	storeSessions map[string]map[string]struct{}
	mutationLock  *native.PathLocks
	closed        bool
	// delegatedRecovered holds, by session state folder, a *sync.Once that finishes the
	// worktrees its delegated runs lost when the last server stopped, the first time the
	// session is resolved.
	delegatedRecovered sync.Map

	// remotes owns the clients and follower goroutines of remote profile_message calls.
	remotes *remote.Hub

	// profileRuns registers profile_message runs in the HTTP run registry; nil means no tracking.
	profileRuns atomic.Pointer[httpapi.ProfileRunStarter]
	// runActive reports whether a run of a session profile is running; set by SetRunActive.
	runActive atomic.Pointer[func(reference, profile string) bool]
	// gate is the mutation gate and session locks, shared with the HTTP handler.
	gate *sessiongate.Gate

	// subagentBuilds holds the managers being built, by storage directory. Building
	// one reads, rewrites and may delete a lot of files, so it runs outside mu; a
	// second caller for the same directory waits on the entry instead of building
	// again (the recovery of a directory must run once).
	subagentBuilds map[string]*subagentBuild
	// newPersistentSubagentManager builds a manager that keeps run manifests; tests
	// replace it to hold a build open.
	newPersistentSubagentManager func(native.SubagentManagerOptions) *native.SubagentManager
	// openStore opens one transcript store; tests replace it to hold a build open.
	openStore func(sessionfs.SessionConfig, transcript.TurnStoreOptions, transcript.DurabilityMode) (*sharedSession, error)
}

// errStoreForgotten is what a caller gets when its session was deleted while its transcript
// store was being opened: the store is not handed out, because nothing caches it any more
// and a second open of the same file would follow.
var errStoreForgotten = &httpapi.StatusError{Code: http.StatusNotFound, Message: "session was deleted"}

// storeEntry is one transcript store being built or built. Opening a store creates
// directories, opens the journal and recovers the transcript tail, so it runs outside
// Resolver.mu: callers of the same key wait on ready, other keys do not wait at all.
//
// leases counts the operations using the store now. Only a store nobody uses leaves the
// cache: two stores of one transcript would each have their own run lock and journal
// lock, and could write it at once. When the count drops to zero, idleTimer drops the
// entry after CatalogConfig.StoreIdle unless a use starts first; idleEpoch tells a timer
// that fires late that a newer idle period has begun. All three are guarded by Resolver.mu.
type storeEntry struct {
	ready     chan struct{}
	shared    *sharedSession
	err       error
	leases    int
	idleEpoch uint64
	idleTimer *time.Timer
}

type sharedSession struct {
	store        *transcript.TurnStore
	staging      string
	durability   transcript.DurabilityMode
	options      transcript.TurnStoreOptions
	runMu        runMutex
	turnStartsMu sync.Mutex
	turnStarts   *turnStartsCache
	// historyMu guards the history block's message count, kept with the block's ETag
	// (historyReferenceFor). An empty historyETag holds nothing.
	historyMu    sync.Mutex
	historyETag  string
	historyCount int
	runners      map[string]*session.Runner

	logger           *slog.Logger
	usageCacheFailed atomic.Bool
}

// sessionKey names one session and profile, like the HTTP run registry's key. It is the
// owner of the sub-agent runs the session starts, so a stop or reset of the session
// reaches exactly those.
func sessionKey(reference, profile string) string {
	return reference + "\x00" + profile
}

// Reasons recorded on handles ended by the user's own actions.
const (
	reasonUserStop      = "user_stop"
	reasonSessionReset  = "session_reset"
	reasonSessionDelete = "session_delete"
)

func (r *Resolver) asyncManagers() []*asyncmgr.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	managers := make([]*asyncmgr.Manager, 0, len(r.asyncHosts))
	for _, host := range r.asyncHosts {
		managers = append(managers, host.Manager())
	}
	return managers
}

func (r *Resolver) subagentManagerList() []*native.SubagentManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	managers := make([]*native.SubagentManager, 0, len(r.subagentManagers))
	for _, manager := range r.subagentManagers {
		managers = append(managers, manager)
	}
	return managers
}

// CancelSessionChildren asks everything a session started that outlives its runs (the
// session's handles of every kind: processes and sub-agent runs, and, through their lineage,
// the work its chains left going in other profiles and sessions) to stop, and reports how
// many were still going. The HTTP layer calls it when the user stops the session. It does
// not wait. A normal end of a run or a compaction never comes here.
func (r *Resolver) CancelSessionChildren(reference, profile string) int {
	owner := sessionKey(reference, profile)
	// First, so that a run of the owner the user stopped that is still going reaches, once it
	// ends, what its calls start after the fan-out below has looked (liveRunStops).
	r.runStops.note(func(o string) bool { return o == owner }, reasonUserStop)
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.CancelOwner(owner, reasonUserStop)
	}
	cancelled += r.cancelSubagentOwner(owner, reasonUserStop)
	return cancelled + r.cancelDescendants(owner, reasonUserStop)
}

// ResetSessionChildren is CancelSessionChildren for a reset: the session's unread
// completion signals are dropped too, because the history that gave them meaning is gone.
func (r *Resolver) ResetSessionChildren(reference, profile string) int {
	owner := sessionKey(reference, profile)
	r.runStops.note(func(o string) bool { return o == owner }, reasonSessionReset)
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.ResetOwner(owner, reasonSessionReset)
	}
	cancelled += r.cancelSubagentOwner(owner, reasonSessionReset)
	// The work the session's chains left in other profiles is stopped, not reset: its own
	// profile's history is not gone, so its handles keep their records and signals.
	return cancelled + r.cancelDescendants(owner, reasonSessionReset)
}

// cancelDescendants is cancelLineage for one key: a session key (the work a profile's chains
// left in other profiles), a run's lineage key (a sub-agent run's or a persistent run's,
// native.NewRunLineageKey) or the key of a run profile_message started.
func (r *Resolver) cancelDescendants(key, reason string) int {
	return r.cancelLineage(func(k string) bool { return k == key }, reason)
}

// deleteDescendants is cancelDescendants for a session delete that reaches work after its fan-out
// (CancelAllSessionChildren) looked, with the same end: handles owned under session (every profile
// of the deleted session) are stopped and their records removed (asyncmgr DeleteLineage), handles
// of other owners and sub-agent runs that are not handles are only stopped, so the records of
// other sessions, also on a storage they share, stay. It does not wait.
func (r *Resolver) deleteDescendants(key, session, reason string) int {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0
	}
	match := func(k string) bool { return k == key }
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.DeleteLineage(match, session, reason)
	}
	for _, manager := range r.subagentManagerList() {
		cancelled += manager.CancelUnmanagedLineage(match, reason)
	}
	return cancelled
}

// cancelLineage asks every live handle and sub-agent run whose lineage has a key that match
// accepts to stop, in every async and sub-agent manager of the resolver (the work may be in
// another session's storage), and reports how many it asked. Owners are not looked at: the
// owner fan-out of the caller reaches its own work. Each stop is a cancel made outside every
// lock; it does not wait. A closed resolver has nothing left to reach.
func (r *Resolver) cancelLineage(match func(key string) bool, reason string) int {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0
	}
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.CancelLineage(match, reason)
	}
	for _, manager := range r.subagentManagerList() {
		cancelled += manager.CancelUnmanagedLineage(match, reason)
	}
	return cancelled
}

// BackgroundWriters implements httpapi.SessionWriters: how many sub-agent runs of the session
// can change files and are going as handles, in every sub-agent manager of the resolver (their
// places are counted in the one run tree, finishSubagentBuild).
func (r *Resolver) BackgroundWriters(reference, profile string) int {
	return r.runTree.Writers(sessionKey(reference, profile))
}

// cancelSubagentOwner reaches the sub-agent runs that are not async handles (a session
// without the async tool, or a run that blocks its call); a handle was already asked to stop
// and counted by its async manager. The reason is the one the async manager is given.
func (r *Resolver) cancelSubagentOwner(owner, reason string) int {
	cancelled := 0
	for _, manager := range r.subagentManagerList() {
		cancelled += manager.CancelUnmanagedOwner(owner, reason)
	}
	return cancelled
}

// NewResolver returns a filesystem-backed resolver. Store and runner instances
// are shared by canonical transcript/staging paths to preserve in-process
// serialization across roots, profiles, and model selections.
func NewResolver(roots []sessionfs.Root, environment map[string]string, catalog CatalogConfig, options ...ResolverOption) (*Resolver, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one session root is required")
	}
	if environment == nil {
		return nil, errors.New("process environment is required")
	}
	if catalog.SnapshotTurns < 0 || catalog.MaxPageTurns < 1 || catalog.MaxInlinePartBytes < 0 || catalog.StoreIdle < 0 {
		return nil, errors.New("invalid session catalog settings")
	}
	copyRoots := append([]sessionfs.Root(nil), roots...)
	copyEnvironment := make(map[string]string, len(environment))
	for name, value := range environment {
		copyEnvironment[name] = value
	}
	resolverOptions := resolverOptions{logger: logging.Discard()}
	for _, option := range options {
		option(&resolverOptions)
	}
	if resolverOptions.leafBudget == nil {
		resolverOptions.leafBudget = tools.NewLeafBudget(serverconfig.DefaultMaxLeafCalls)
	}
	return &Resolver{
		logger:            resolverOptions.logger,
		roots:             copyRoots,
		environment:       copyEnvironment,
		catalog:           catalog,
		stores:            make(map[string]*storeEntry),
		profileTurnCounts: make(map[string]profileTurnsEntry),
		remotes:           remote.NewHub(func(name string) string { return copyEnvironment[name] }),
		subagentManagers:  make(map[string]*native.SubagentManager),
		leafBudget:        resolverOptions.leafBudget,
		asyncHosts:        make(map[string]*native.AsyncHost),
		asyncBuilds:       make(map[string]chan struct{}),
		openAsyncHost:     native.NewAsyncHost,
		mcpManagers:       make(map[string]*mcpclient.Manager),
		taskStores:        make(map[string]*native.TaskStore),
		recordsStores:     make(map[string]*native.RecordsStore),
		storeSessions:     make(map[string]map[string]struct{}),
		mutationLock:      native.NewPathLocks(),
		gate:              &sessiongate.Gate{},
		openStore:         openSharedSession,

		subagentBuilds:               make(map[string]*subagentBuild),
		newPersistentSubagentManager: native.NewPersistentSubagentManager,
	}, nil
}

// LockSession implements httpapi.SessionProcessLocker. It resolves the
// profile's storage paths and locks the selected transcript for the length of
// the run.
//
// It does not take the session settings lock. That lock is one file for the
// whole session, so holding it, even only while the paths are read, adds
// nothing here: setting.json is replaced atomically, so a read never sees a
// torn file, and a profile switch writes the active profile and the pinned
// storage paths of both profiles in one replace. A run therefore resolves the
// same transcript before or after any settings write. A test that removed the
// lock from run start found no scenario where it mattered (cross-process
// tests in session_lock_processes_test.go).
func (r *Resolver) LockSession(ctx context.Context, reference, profile, modelPath string) (func(), error) {
	return r.lockSession(ctx, reference, profile, modelPath, false)
}

// TryLockSession is LockSession without the wait: it returns httpapi.ErrSessionBusy at once
// when another run holds the transcript.
func (r *Resolver) TryLockSession(ctx context.Context, reference, profile, modelPath string) (func(), error) {
	return r.lockSession(ctx, reference, profile, modelPath, true)
}

func (r *Resolver) lockSession(ctx context.Context, reference, profile, modelPath string, try bool) (func(), error) {
	location, err := r.resolveLockLocation(ctx, reference)
	if err != nil {
		return nil, err
	}
	// Locking needs only the storage paths. The model file is not read until the
	// run itself needs it, so a broken model.json cannot block a run from
	// reaching the point where its input is stored.
	config, stamp, err := sessionfs.LoadSessionSettingsStamped(location, profile, modelPath)
	if err != nil {
		return nil, mapSessionError(err)
	}
	r.logConfigWarnings(location.Reference, config)
	loaded := time.Now()
	transcriptLockPath, err := sessionlock.PathFor(config.MessagesPath)
	if err != nil {
		return nil, err
	}
	var transcriptLock *sessionlock.Lock
	if try {
		transcriptLock, err = sessionlock.TryAcquire(ctx, transcriptLockPath)
		if errors.Is(err, sessionlock.ErrBusy) {
			return nil, httpapi.ErrSessionBusy
		}
	} else {
		transcriptLock, err = sessionlock.Acquire(ctx, transcriptLockPath)
	}
	if err != nil {
		return nil, fmt.Errorf("lock session transcript: %w", err)
	}
	key := lockedConfigKey{file: location.File, profile: profile, modelPath: modelPath}
	// After a wait the config may be old, and Resolve loads its own under the lock.
	if !transcriptLock.Waited() {
		r.lockedConfigs.put(key, lockedConfig{config: config, stamp: stamp, loaded: loaded})
	}
	return func() {
		r.lockedConfigs.drop(key)
		_ = transcriptLock.Close()
	}, nil
}

func (r *Resolver) logConfigWarnings(reference string, config sessionfs.SessionConfig) {
	if r.logger == nil {
		return
	}
	for _, warning := range config.Warnings {
		r.logger.Warn(warning, "session", reference, "profile", config.Profile)
	}
}

// LockSessionSettings serializes writes to setting.json across processes.
func (r *Resolver) LockSessionSettings(ctx context.Context, reference string) (func(), error) {
	location, err := r.resolveLockLocation(ctx, reference)
	if err != nil {
		return nil, err
	}
	return r.acquireSettingLock(ctx, location)
}

func (r *Resolver) resolveLockLocation(ctx context.Context, reference string) (sessionfs.Location, error) {
	if ctx == nil {
		return sessionfs.Location{}, errors.New("session lock context is required")
	}
	if err := ctx.Err(); err != nil {
		return sessionfs.Location{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return sessionfs.Location{}, mapSessionError(err)
	}
	info, err := os.Stat(location.File)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionfs.Location{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "session not found"}
		}
		return sessionfs.Location{}, fmt.Errorf("inspect session file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return sessionfs.Location{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "session not found"}
	}
	return location, nil
}

func (r *Resolver) acquireSettingLock(ctx context.Context, location sessionfs.Location) (func(), error) {
	path, err := sessionlock.PathFor(location.SettingFile())
	if err != nil {
		return nil, err
	}
	lock, err := sessionlock.Acquire(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("lock session settings: %w", err)
	}
	return func() { _ = lock.Close() }, nil
}

// Resolve implements httpapi.SessionResolver.
func (r *Resolver) Resolve(ctx context.Context, reference, profile, modelPath string) (httpapi.ResolvedSession, error) {
	if ctx == nil {
		return httpapi.ResolvedSession{}, errors.New("session resolution context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.ResolvedSession{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.ResolvedSession{}, mapSessionError(err)
	}
	// Only the settings are read here. Like the TypeScript backend, the model
	// file, the tool servers and the credentials are not needed to resolve a
	// session (to attach to a run, stop one, or accept a message); they are
	// prepared by the run itself, after its user input is stored.
	config, reused := r.lockedConfigs.take(lockedConfigKey{file: location.File, profile: profile, modelPath: modelPath}, time.Now())
	if !reused {
		config, err = sessionfs.LoadSessionSettings(location, profile, modelPath)
		if err != nil {
			return httpapi.ResolvedSession{}, mapSessionError(err)
		}
		r.logConfigWarnings(location.Reference, config)
	}
	r.recoverDelegatedWorktrees(ctx, config)
	// The runner leases the store again for each operation (boundRunner.hold); this
	// open only reports a store that cannot be used before the run is registered.
	shared, release, err := r.sharedStore(config)
	if err != nil {
		return httpapi.ResolvedSession{}, err
	}
	release()
	return httpapi.ResolvedSession{
		Reference: location.Reference,
		Profile:   config.Profile,
		Runner:    boundRunner{resolver: r, shared: shared, config: config, reference: location.Reference},
	}, nil
}

// recoverDelegatedWorktrees finishes, once per session state folder, the worktrees of the
// delegated profile runs (profile_message into a profile with sandbox.isolation.mode worktree)
// that the last server lost when it stopped: their work goes to their branches and the folders
// are removed (native.RecoverDelegatedWorktrees). Every profile of a session shares the
// folder, so the first resolve of any of them does it.
func (r *Resolver) recoverDelegatedWorktrees(ctx context.Context, config sessionfs.SessionConfig) {
	if config.StateDir == "" {
		return
	}
	once, _ := r.delegatedRecovered.LoadOrStore(filepath.Clean(config.StateDir), &sync.Once{})
	once.(*sync.Once).Do(func() {
		native.RecoverDelegatedWorktrees(context.WithoutCancel(ctx), config.StateDir, config.NativeSubagent, r.logger)
	})
}

// preparedRun is everything a run needs beyond the session settings.
type preparedRun struct {
	config       sessionfs.SessionConfig
	runner       *session.Runner
	toolCatalog  *tools.Catalog
	taskStore    *native.TaskStore
	recordsStore *native.RecordsStore
}

// prepareRun reads the model file, builds the tool catalog (connecting MCP
// servers), resolves the credential and returns the shared runner. The steps and
// their order follow the TypeScript run: model, then tools, then the client.
// The caller holds shared.runMu, which also guards shared.runners.
func (r *Resolver) prepareRun(ctx context.Context, shared *sharedSession, settings sessionfs.SessionConfig, mcpScope *mcpScopeHold) (preparedRun, error) {
	config, err := settings.WithModel()
	if err != nil {
		return preparedRun{}, err
	}
	toolCatalog, taskStore, recordsStore, err := r.buildToolCatalog(ctx, config, mcpScope)
	if err != nil {
		return preparedRun{}, err
	}
	apiKey, err := r.providerAPIKey(config)
	if err != nil {
		return preparedRun{}, err
	}
	if config.MaxRunCostUSD > 0 && config.Model.Pricing == nil {
		return preparedRun{}, &httpapi.StatusError{
			Code:    http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("max_run_cost_usd is set, but the model file %s has no pricing, so the run's cost cannot be counted", config.ModelPath),
		}
	}
	if config.Budget.CostCapped() && config.Model.Pricing == nil {
		return preparedRun{}, &httpapi.StatusError{
			Code:    http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("budget caps a cost in US dollars, but the model file %s has no pricing, so the run's cost cannot be counted", config.ModelPath),
		}
	}
	// The provider clients are built only for a new runner; a cached runner keeps its own.
	plan := r.planRunStreamer(config, apiKey)
	runnerKey := fingerprintRunner(config, apiKey) + plan.identity
	runner := shared.runners[runnerKey]
	if runner == nil {
		runner, err = session.NewRunner(shared.store, r.buildRunStreamer(plan), runnerModelSettings(config))
		if err != nil {
			return preparedRun{}, fmt.Errorf("create session runner: %w", err)
		}
		shared.runners[runnerKey] = runner
	}
	return preparedRun{config: config, runner: runner, toolCatalog: toolCatalog, taskStore: taskStore, recordsStore: recordsStore}, nil
}

// providerAPIKey returns the key handed to the provider client for a loaded model, and
// fails the run before it starts when the credential cannot be used.
func (r *Resolver) providerAPIKey(config sessionfs.SessionConfig) (string, error) {
	if config.Model.AuthMode == "openai_oauth" {
		// The token itself is read from the file on every request, so it is not part of
		// the runner's identity; the file path and the OAuth settings are.
		if err := checkOAuthCredential(config, r.logger); err != nil {
			return "", err
		}
		return oauthPlaceholderKey, nil
	}
	if config.Model.API == sessionfs.APIAnthropicMessages && anthropicHoldsCredential(config) {
		// The adapter reads its token from upstream.credential_file on every request and
		// ignores the key it is handed, so api_key_env needs no value.
		return credentialFilePlaceholderKey, nil
	}
	apiKey := strings.TrimSpace(r.environment[config.Model.APIKeyEnv])
	if apiKey == "" {
		return "", &httpapi.StatusError{
			Code:    http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("environment variable %s is not set", config.Model.APIKeyEnv),
		}
	}
	return apiKey, nil
}

// newProviderStreamer builds the provider client of a loaded model: the OAuth
// transport when the model uses it, wrapped by the HTTP debug logger when that is on.
// A model in api anthropic_messages or chat_completions gets the same Responses client
// with an in-process translator in place of the network; the translator's own calls
// use the network, OAuth-free, and the HTTP debug logger records them.
func (r *Resolver) newProviderStreamer(config sessionfs.SessionConfig, apiKey string) *openaiapi.Client {
	httpClient := providerHTTPClient(config, r.logger)
	baseURL := config.Model.BaseURL
	if translator := providerTranslator(config, httpClient, r.logger); translator != nil {
		httpClient = &http.Client{Transport: &inprocess.Transport{Handler: translator, Logger: r.logger}}
		baseURL = inProcessBaseURL
	}
	retry := providerRetryConfig(config.ProviderRetry, r.logger)
	return openaiapi.NewClient(openaiapi.Config{APIKey: apiKey, BaseURL: baseURL, HTTPClient: httpClient, Retry: &retry, IdleTimeout: time.Duration(config.ProviderStream.IdleTimeoutMS) * time.Millisecond})
}

// providerHTTPClient returns nil when nothing changes the default client: no OAuth, no
// HTTP debug, no header timeout and the default network. The base transport is built once and shared by the
// OAuth wrapper and the debug wrapper.
func providerHTTPClient(config sessionfs.SessionConfig, logger *slog.Logger) *http.Client {
	var httpClient *http.Client
	var network http.RoundTripper = http.DefaultTransport
	var base *http.Transport
	if config.ProviderNetwork.Network != "" && config.ProviderNetwork.Network != "tcp" {
		base = newNetworkTransport(config.ProviderNetwork.Network, providerDial)
	}
	if config.ProviderStream.HeaderTimeoutMS > 0 {
		// The wrappers below (OAuth, HTTP debug) and an in-process translator's own calls
		// all go through this transport, so they all get the header timeout.
		if base == nil {
			base = http.DefaultTransport.(*http.Transport).Clone()
		}
		base.ResponseHeaderTimeout = time.Duration(config.ProviderStream.HeaderTimeoutMS) * time.Millisecond
	}
	if base != nil {
		network = base
		httpClient = &http.Client{Transport: network}
	}
	if config.Model.AuthMode == "openai_oauth" {
		network = oauthTransport(config, network)
		httpClient = &http.Client{Transport: network}
	}
	if config.HTTPDebug.Enabled {
		transport := httpdebug.NewTransport(config.StoragePaths.HTTPDebug, httpdebug.Settings{
			Logger:                 logger,
			Enabled:                true,
			IncludeRequestHeaders:  config.HTTPDebug.IncludeRequestHeaders,
			IncludeRequestBody:     config.HTTPDebug.IncludeRequestBody,
			IncludeResponseHeaders: config.HTTPDebug.IncludeResponseHeaders,
			IncludeResponseBody:    config.HTTPDebug.IncludeResponseBody,
			MaxBodyBytes:           config.HTTPDebug.MaxBodyBytes,
			MaxTotalBytes:          config.HTTPDebug.MaxTotalBytes,
		}, network)
		httpClient = &http.Client{Transport: transport}
	}
	if config.Model.AuthMode == "openai_oauth" {
		httpClient.CheckRedirect = refuseCrossOriginRedirect
	}
	return httpClient
}

// ListTools implements httpapi.SessionToolLister without requiring a provider
// API key or creating a session runner.
func (r *Resolver) ListTools(ctx context.Context, reference, profile string) (httpapi.SessionTools, error) {
	if ctx == nil {
		return httpapi.SessionTools{}, errors.New("session resolution context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.SessionTools{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionTools{}, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionTools{}, mapSessionError(err)
	}
	registered, _, _, err := r.buildRegisteredCatalog(ctx, config, nil)
	if err != nil {
		return httpapi.SessionTools{}, err
	}
	exposed, hidden, err := applyProfileTools(config, registered)
	if err != nil {
		return httpapi.SessionTools{}, mapSessionError(err)
	}
	return httpapi.SessionTools{Tools: exposed.RouteDefinitions(), Hidden: hidden}, nil
}

// buildToolCatalog is the catalog the profile's model sees: every registered tool, narrowed
// by the profile's tools.allow when it has one.
func (r *Resolver) buildToolCatalog(ctx context.Context, config sessionfs.SessionConfig, mcpScope *mcpScopeHold) (*tools.Catalog, *native.TaskStore, *native.RecordsStore, error) {
	registered, taskStore, recordsStore, err := r.buildRegisteredCatalog(ctx, config, mcpScope)
	if err != nil {
		return nil, nil, nil, err
	}
	// The profile narrows what its own model sees, not what a child may use: the
	// sub-agent tool built its agents' catalogs from the registered tools already.
	toolCatalog, _, err := applyProfileTools(config, registered)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	return toolCatalog, taskStore, recordsStore, nil
}

// buildRegisteredCatalog builds every tool the session registers (native, then MCP). mcpScope is
// nil for a run in the profile's own sandbox, else the run's hold of the manager of its narrowed
// MCP sandbox (runMCPManager).
func (r *Resolver) buildRegisteredCatalog(ctx context.Context, config sessionfs.SessionConfig, mcpScope *mcpScopeHold) (*tools.Catalog, *native.TaskStore, *native.RecordsStore, error) {
	parts, taskStore, recordsStore, err := r.acquireCatalogParts(ctx, config, mcpScope)
	if err != nil {
		return nil, nil, nil, err
	}
	registered, err := assembleRegisteredCatalog(config, parts, func(message string) {
		if r.logger != nil {
			r.logger.Warn(message, "profile", config.Profile)
		}
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return registered, taskStore, recordsStore, nil
}

// acquireCatalogParts takes from the resolver and the outside world what the registered catalog
// is assembled from, in this order: the shared task and records stores, the native catalog (with
// the subagent manager and async resources it registers with), the profile's MCP manager (the
// shared one, or the narrowing's for mcpScope) with its wrapper and route key set and its servers
// started and listed, then each agent toolset's manager and servers. Every side effect of a
// catalog build is here; assembleRegisteredCatalog has none.
func (r *Resolver) acquireCatalogParts(ctx context.Context, config sessionfs.SessionConfig, mcpScope *mcpScopeHold) (catalogParts, *native.TaskStore, *native.RecordsStore, error) {
	taskStore := r.sharedTaskStore(config)
	recordsStore := r.sharedRecordsStore(config)
	// Its children's catalogs are built by assembleRegisteredCatalog, once, from every part.
	subagentMCP := native.NewAssembledSubagentMCP(config.Hooks, config.Model.APIKeyEnv)
	// One lazy set per prepare, passed to both the native pass (extension tools) and the MCP pass
	// below. Nil when the profile has no tools.lazy.
	lazy := openLazySet(config)
	sections, err := native.BuildCatalogSections(config, r.mutationLock, taskStore, recordsStore, r.subagentLauncherFor(config), subagentMCP, r.sharedSubagentManager(config), r, profileMessenger{resolver: r}, r, lazy)
	if err != nil {
		return catalogParts{}, nil, nil, mapSessionError(err)
	}
	mcpManager, err := r.runMCPManager(config, mcpScope)
	if err != nil {
		return catalogParts{}, nil, nil, err
	}
	mcpManager.SetCommandWrapper(mcpSandboxWrapper(config))
	// A run in a worktree of its own calls these servers' tools on servers of its own.
	mcpManager.SetRouteKey("")
	// A server whose file enables sampling is answered, during a call of these tools, with the
	// model of this config: the run being prepared, or what a background child or script of it
	// calls later. The manager is shared by the profile's runs and listings, so the sampler goes
	// with the listed tools and is never set on the manager, where the last listing (another
	// run, a model choice, a tool listing, a remote start) would answer for every run.
	mcpTools, err := mcpManager.LoadToolsSampled(ctx, config.MCPServers, config.WorkingDir, r.mcpSampler(config))
	if err != nil {
		return catalogParts{}, nil, nil, mapSessionError(err)
	}
	toolsetMCP, err := r.loadAgentToolsetMCP(ctx, config)
	if err != nil {
		return catalogParts{}, nil, nil, err
	}
	return catalogParts{
		sections: sections, subagentMCP: subagentMCP, lazy: lazy,
		mcpTools: mcpTools, toolsetMCP: toolsetMCP, runMCP: r.runMCPLoader(config),
	}, taskStore, recordsStore, nil
}

// AsyncHost implements native.AsyncResources: one async manager per tool storage folder,
// shared by every session that uses it.
func (r *Resolver) AsyncHost(config native.AsyncHostConfig) (*native.AsyncHost, error) {
	key := filepath.Clean(config.Dir)
	for {
		r.mu.Lock()
		if host := r.asyncHosts[key]; host != nil {
			r.mu.Unlock()
			return host, nil
		}
		if r.closed {
			r.mu.Unlock()
			return nil, errors.New("resolver is closed")
		}
		if done := r.asyncBuilds[key]; done != nil {
			// Opening runs the recovery of the directory, which must run once: wait for
			// the opener, then look again.
			r.mu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		r.asyncBuilds[key] = done
		r.mu.Unlock()
		return r.finishAsyncHost(key, done, config)
	}
}

// finishAsyncHost opens the manager without holding mu (it reads and rewrites files),
// then publishes it. A resolver that closed meanwhile does not get it: Close never saw
// this host, so it is closed here.
func (r *Resolver) finishAsyncHost(key string, done chan struct{}, config native.AsyncHostConfig) (published *native.AsyncHost, err error) {
	var host *native.AsyncHost
	defer func() {
		r.mu.Lock()
		delete(r.asyncBuilds, key)
		closed := r.closed
		if host != nil && !closed {
			r.asyncHosts[key] = host
			published = host
		}
		r.mu.Unlock()
		// Closed before done is: Resolver.Close waits for done, so the host's last file
		// writes (its final index) do not outlive it.
		if host != nil && closed {
			host.Close()
			published, err = nil, errors.New("resolver is closed")
		}
		close(done)
	}()
	host, err = r.openAsyncHost(key, config.Config, r.logger)
	if host != nil {
		// A handle's end may drain the lineage a narrowed run's MCP manager (mcpScopeHold) or an
		// ended message run's tree node (releaseNodeAfter) waits for.
		host.Manager().SetLineageEnd(r.lineageWorkEnded)
	}
	return nil, err
}

// sharedSubagentManager returns the manager that owns the child runs of one storage
// directory, so a run started in one request can be read in a later one.
func (r *Resolver) sharedSubagentManager(config sessionfs.SessionConfig) *native.SubagentManager {
	if config.NativeSubagent == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeSubagent.StorageDir)
	for {
		r.mu.Lock()
		if manager := r.subagentManagers[storageKey]; manager != nil {
			r.mu.Unlock()
			return manager
		}
		if r.closed {
			r.mu.Unlock()
			return nil
		}
		if build := r.subagentBuilds[storageKey]; build != nil {
			// Someone is building this directory's manager: wait for them, then look
			// again (the build may have ended because the resolver closed).
			r.mu.Unlock()
			<-build.done
			continue
		}
		build := &subagentBuild{done: make(chan struct{})}
		r.subagentBuilds[storageKey] = build
		r.mu.Unlock()
		return r.finishSubagentBuild(storageKey, build, config)
	}
}

// existingSubagentManager returns the manager of a storage directory if one is built, else nil.
// Unlike sharedSubagentManager it never builds one.
func (r *Resolver) existingSubagentManager(storageDir string) *native.SubagentManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subagentManagers[filepath.Clean(storageDir)]
}

// subagentBuild is one manager being built; done closes when it is in the map or the
// build was given up.
type subagentBuild struct {
	done chan struct{}
}

// finishSubagentBuild builds the manager without holding mu, then publishes it. A
// resolver that closed meanwhile does not get it: Close never saw this manager, so it
// is closed here.
func (r *Resolver) finishSubagentBuild(storageKey string, build *subagentBuild, config sessionfs.SessionConfig) (published *native.SubagentManager) {
	var manager *native.SubagentManager
	defer func() {
		// Runs on every exit, a panic in the builder included, so waiters are released.
		r.mu.Lock()
		delete(r.subagentBuilds, storageKey)
		closed := r.closed
		if manager != nil && !closed {
			r.subagentManagers[storageKey] = manager
			published = manager
		}
		r.mu.Unlock()
		// Closed before done is, so the manager's close does not outlive Resolver.Close.
		if manager != nil && closed {
			_ = manager.Close()
		}
		close(build.done)
	}()
	if config.NativeSubagent.PersistRuns {
		manager = r.newPersistentSubagentManager(native.SubagentManagerOptions{
			StorageDir: config.NativeSubagent.StorageDir, Keep: int(config.NativeSubagent.KeepFinished), Logger: r.logger,
			DeleteTranscripts: config.NativeSubagent.DeleteTranscripts, Worktree: config.NativeSubagent.Worktree,
		})
	} else {
		manager = native.NewSubagentManager()
		if config.NativeSubagent.DeleteTranscripts {
			manager.DeleteTranscriptsUnder(config.NativeSubagent.StorageDir)
		}
	}
	// Before it is published, so its first start already counts in the process's run tree.
	manager.SetRunTree(&r.runTree)
	// A run stopped in this manager stops the work it started in other profiles, in whichever
	// manager that work is (cancelLineage).
	manager.SetStopCascade(func(key, reason string) { r.cancelDescendants(key, reason) })
	// A run's end may drain the lineage a narrowed run's MCP manager (mcpScopeHold) or an ended
	// message run's tree node (releaseNodeAfter) waits for.
	manager.SetRunEnd(r.lineageWorkEnded)
	return // published is set by the deferred function
}

func (r *Resolver) sharedTaskStore(config sessionfs.SessionConfig) *native.TaskStore {
	if config.NativeTask == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeTask.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store := r.taskStores[storageKey]; store != nil {
		r.noteStoreSession("task", storageKey, config.SettingPath)
		return store
	}
	if r.closed {
		return nil
	}
	r.noteStoreSession("task", storageKey, config.SettingPath)
	store := native.NewTaskStore(storageKey)
	r.taskStores[storageKey] = store
	return store
}

func (r *Resolver) sharedRecordsStore(config sessionfs.SessionConfig) *native.RecordsStore {
	if config.NativeRecords == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeRecords.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store := r.recordsStores[storageKey]; store != nil {
		r.noteStoreSession("records", storageKey, config.SettingPath)
		return store
	}
	if r.closed {
		return nil
	}
	r.noteStoreSession("records", storageKey, config.SettingPath)
	store := native.NewRecordsStore(storageKey, r.mutationLock)
	r.recordsStores[storageKey] = store
	return store
}

// noteStoreSession records that a session file uses a task or records store. The
// caller holds r.mu.
func (r *Resolver) noteStoreSession(kind, storageKey, settingPath string) {
	key := kind + "\x00" + storageKey
	sessions := r.storeSessions[key]
	if sessions == nil {
		sessions = make(map[string]struct{})
		r.storeSessions[key] = sessions
	}
	sessions[filepath.Clean(settingPath)] = struct{}{}
}

// sharedMCPManager returns the MCP manager of one session and profile. A manager closes
// every connection a run does not list, so profiles that pick different environments
// of one server (browser-headed/betterwright, browser-headless/betterwright) need
// separate managers, or each run would close the other's connection.
func (r *Resolver) sharedMCPManager(config sessionfs.SessionConfig) (*mcpclient.Manager, error) {
	return r.mcpManagerFor(filepath.Clean(config.SettingPath) + "\x00" + config.Profile)
}

// runMCPManager is the manager a run of config lists and calls its session's MCP servers on.
// scope nil: the profile's shared manager (sharedMCPManager). Otherwise scope is the run's hold of
// the manager of its narrowing (holdMCPScope): its stdio servers start in the sandbox the run's
// profile_message caller narrowed, and the profile's shared servers, which its own runs and what
// they left running use, are neither restarted nor rewrapped by it (D10, D20). The manager lives
// while it is held; the last release closes it (mcpScopeHold), and a session delete or shutdown
// closes it with the session's managers (releaseSessionChildren, Close).
func (r *Resolver) runMCPManager(config sessionfs.SessionConfig, scope *mcpScopeHold) (*mcpclient.Manager, error) {
	if scope == nil {
		return r.sharedMCPManager(config)
	}
	return scope.manager, nil
}

// Resources implements native.ToolsetResources: the shared stores of a toolset that a
// sub-agent names, keyed by that toolset's own storage directories.
func (r *Resolver) Resources(config sessionfs.SessionConfig) (*native.TaskStore, *native.RecordsStore) {
	return r.sharedTaskStore(config), r.sharedRecordsStore(config)
}

// loadAgentToolsetMCP connects the MCP servers of every toolset a sub-agent names and
// returns their tools by native.ToolsetMCPKey. Each key has its own manager, so a server that
// two toolsets use differently is not closed by the other's run. The servers start in the
// sandbox like the session's own (U4): an agent whose own sandbox covers mcp gets servers of
// its own in that sandbox; the other agents of a toolset share its servers in the toolset's.
func (r *Resolver) loadAgentToolsetMCP(ctx context.Context, config sessionfs.SessionConfig) (map[string]native.ToolsetMCP, error) {
	views := native.NewAgentToolConfigs(config).MCP
	if len(views) == 0 {
		return nil, nil
	}
	loaded := make(map[string]native.ToolsetMCP, len(views))
	for _, view := range views {
		manager, err := r.mcpManagerFor(filepath.Clean(config.SettingPath) + "\x00" + config.Profile + "\x00toolset\x00" + view.Key)
		if err != nil {
			return nil, err
		}
		manager.SetCommandWrapper(mcpSandboxWrapper(view.Config))
		manager.SetRouteKey(view.Key)
		mcpTools, err := manager.LoadTools(ctx, view.Config.MCPServers, view.Config.WorkingDir)
		if err != nil {
			return nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: toolset %s: %v", config.SettingPath, view.Toolset, err)})
		}
		// Raw: the subagent tool gates each copy with the owner's lazy set, then hooks it with these hooks.
		loaded[view.Key] = native.ToolsetMCP{Tools: mcpTools, Hooks: view.Config.Hooks}
	}
	return loaded, nil
}

func (r *Resolver) mcpManagerFor(key string) (*mcpclient.Manager, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	return r.mcpManagerForLocked(key), nil
}

// mcpManagerForLocked is mcpManagerFor with mu held and the resolver open.
func (r *Resolver) mcpManagerForLocked(key string) *mcpclient.Manager {
	if manager := r.mcpManagers[key]; manager != nil {
		return manager
	}
	manager := mcpclient.NewManager(r.environment)
	manager.SetLogger(r.logger)
	r.mcpManagers[key] = manager
	return manager
}

// sharedStore returns the transcript store cached for config, opening it when it is not
// cached, and a release that ends this use of it. The caller calls release exactly once,
// when it no longer touches the store; until then the store is not dropped as idle. An
// error comes with a release that does nothing.
func (r *Resolver) sharedStore(config sessionfs.SessionConfig) (*sharedSession, func(), error) {
	storeOptions := transcript.TurnStoreOptions{
		Logger:                     r.logger,
		MaxItemBytes:               config.Persistence.MaxItemBytes,
		ResponseMetadata:           transcript.ResponseMetadataMode(config.Persistence.ResponseMetadata),
		MessagesBackupPath:         config.StoragePaths.MessagesBackup,
		CompactionCachePath:        config.StoragePaths.CompactionCache,
		UsageCachePath:             config.StoragePaths.UsageCache,
		CursorCachePath:            config.StoragePaths.CursorCache,
		RunErrorCachePath:          config.StoragePaths.RunErrorCache,
		CursorCacheCheckpointBytes: config.Persistence.CursorCacheCheckpointBytes,
		StateContextPath:           config.StoragePaths.StateContext,
	}
	storageKey := filepath.Clean(config.MessagesPath)
	durability := transcript.DurabilityMode(config.Persistence.Durability)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, noRelease, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	entry := r.stores[storageKey]
	if entry == nil {
		entry = &storeEntry{ready: make(chan struct{})}
		r.stores[storageKey] = entry
		r.leaseStoreLocked(entry)
		r.mu.Unlock()
		entry.shared, entry.err = r.buildStore(config, storeOptions, durability)
		r.mu.Lock()
		if entry.err != nil && r.stores[storageKey] == entry {
			delete(r.stores, storageKey)
		}
		if r.stores[storageKey] != entry && entry.err == nil {
			entry.shared, entry.err = nil, errStoreForgotten
		}
		close(entry.ready)
		r.mu.Unlock()
		release := r.storeRelease(storageKey, entry)
		if entry.err != nil {
			release()
			return nil, noRelease, entry.err
		}
		return entry.shared, release, nil
	}
	r.leaseStoreLocked(entry)
	r.mu.Unlock()
	release := r.storeRelease(storageKey, entry)
	<-entry.ready
	if entry.err != nil {
		release()
		return nil, noRelease, entry.err
	}
	shared := entry.shared
	if shared.options != storeOptions || shared.staging != filepath.Clean(config.StagingDir) || shared.durability != durability {
		release()
		return nil, noRelease, &httpapi.StatusError{
			Code:    http.StatusUnprocessableEntity,
			Message: "session path profiles share transcript storage but have incompatible staging or persistence settings",
		}
	}
	return shared, release, nil
}

// noRelease is the release handed out with an error: nothing was leased.
func noRelease() {}

// leaseStoreLocked starts one use of entry and cancels a pending idle drop. The caller
// holds r.mu.
func (r *Resolver) leaseStoreLocked(entry *storeEntry) {
	entry.leases++
	if entry.idleTimer != nil {
		entry.idleTimer.Stop()
		entry.idleTimer = nil
	}
}

// storeRelease returns the release of one use of entry. A second call does nothing, so a
// caller that releases twice cannot end another caller's use.
func (r *Resolver) storeRelease(storageKey string, entry *storeEntry) func() {
	var once sync.Once
	return func() { once.Do(func() { r.releaseStore(storageKey, entry) }) }
}

// releaseStore ends one use of entry. When it was the last and the entry is still the
// cached one, the entry is dropped after the idle time unless a use starts first. A store
// dropped this way is opened again, with recovery, by its next use.
func (r *Resolver) releaseStore(storageKey string, entry *storeEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.leases--
	if entry.leases > 0 || r.closed || r.catalog.StoreIdle <= 0 || r.stores[storageKey] != entry {
		return
	}
	entry.idleEpoch++
	epoch := entry.idleEpoch
	entry.idleTimer = time.AfterFunc(r.catalog.StoreIdle, func() { r.dropIdleStore(storageKey, entry, epoch) })
}

// dropIdleStore removes entry from the cache if nobody has used it since the idle period
// epoch began.
func (r *Resolver) dropIdleStore(storageKey string, entry *storeEntry, epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stores[storageKey] == entry && entry.leases == 0 && entry.idleEpoch == epoch {
		entry.idleTimer = nil
		delete(r.stores, storageKey)
	}
}

// buildStore runs the store opener and turns a panic into an error, so the callers waiting
// on the entry are always released.
func (r *Resolver) buildStore(config sessionfs.SessionConfig, storeOptions transcript.TurnStoreOptions, durability transcript.DurabilityMode) (shared *sharedSession, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			shared, err = nil, fmt.Errorf("open transcript store: panic: %v", recovered)
		}
	}()
	shared, err = r.openStore(config, storeOptions, durability)
	if shared != nil {
		shared.logger = r.logger
	}
	return shared, err
}

func openSharedSession(config sessionfs.SessionConfig, storeOptions transcript.TurnStoreOptions, durability transcript.DurabilityMode) (*sharedSession, error) {
	journal, err := transcript.NewJournal(config.MessagesPath, durability)
	if err != nil {
		return nil, fmt.Errorf("create transcript journal: %w", err)
	}
	store, err := transcript.NewTurnStore(journal, config.StagingDir, storeOptions)
	if err != nil {
		return nil, fmt.Errorf("create transcript turn store: %w", err)
	}
	if _, err := store.Recover(); err != nil {
		return nil, fmt.Errorf("recover session transcript: %w", err)
	}
	return &sharedSession{
		store:      store,
		staging:    filepath.Clean(config.StagingDir),
		durability: durability,
		options:    storeOptions,
		runners:    make(map[string]*session.Runner),
	}, nil
}

// Close prevents new resolutions and stops the runs, handles and servers it started.
func (r *Resolver) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	for _, entry := range r.stores {
		if entry.idleTimer != nil {
			entry.idleTimer.Stop()
			entry.idleTimer = nil
		}
	}
	mcpManagers := make([]*mcpclient.Manager, 0, len(r.mcpManagers))
	for _, manager := range r.mcpManagers {
		mcpManagers = append(mcpManagers, manager)
	}
	subagentManagers := make([]*native.SubagentManager, 0, len(r.subagentManagers))
	for _, manager := range r.subagentManagers {
		subagentManagers = append(subagentManagers, manager)
	}
	asyncHosts := make([]*native.AsyncHost, 0, len(r.asyncHosts))
	for _, host := range r.asyncHosts {
		asyncHosts = append(asyncHosts, host)
	}
	// A manager being built is not in the map yet. Its builder closes it (the resolver
	// is closed), but Close still waits for the build to end, as it did when the build
	// held mu: no file work of a build outlives Close.
	pending := make([]chan struct{}, 0, len(r.subagentBuilds))
	for _, build := range r.subagentBuilds {
		pending = append(pending, build.done)
	}
	for _, done := range r.asyncBuilds {
		pending = append(pending, done)
	}
	r.mu.Unlock()
	for _, done := range pending {
		<-done
	}
	// Remote followers first: they report their end to the async managers closed below, and
	// closing the hub sends each remote run a stop.
	r.remotes.Close()
	// Children first: they may be using MCP tools the next loop closes.
	for _, manager := range subagentManagers {
		_ = manager.Close()
	}
	// Sub-agents first: they report their end to the async manager. The hosts are separate
	// folders and each may wait up to its longest stop, so they close together: shutdown
	// waits for the slowest host, not for the sum.
	var closing sync.WaitGroup
	for _, host := range asyncHosts {
		closing.Go(host.Close)
	}
	closing.Wait()
	for _, manager := range mcpManagers {
		_ = manager.Close()
	}
}

func mapSessionError(err error) error {
	var resolveErr *sessionfs.ResolveError
	if errors.As(err, &resolveErr) {
		status := http.StatusInternalServerError
		switch resolveErr.Kind {
		case sessionfs.ResolveInvalidReference, sessionfs.ResolveAmbiguous:
			status = http.StatusBadRequest
		case sessionfs.ResolveRootNotFound, sessionfs.ResolveSessionNotFound:
			status = http.StatusNotFound
		}
		return &httpapi.StatusError{Code: status, Message: resolveErr.Message}
	}
	var clientErr *sessionfs.ClientError
	if errors.As(err, &clientErr) {
		return &httpapi.StatusError{Code: http.StatusBadRequest, Message: clientErr.Message}
	}
	var configErr *sessionfs.ConfigError
	if errors.As(err, &configErr) {
		return &httpapi.StatusError{Code: http.StatusUnprocessableEntity, Message: configErr.Message}
	}
	return err
}

func fingerprintRunner(config sessionfs.SessionConfig, apiKey string) string {
	identity := struct {
		ModelPath    string
		Provider     string
		Model        string
		APIKeyEnv    string
		APIKeyHash   string
		BaseURL      string
		AuthMode     string
		Request      json.RawMessage
		HostedTools  []json.RawMessage
		PromptRole   string
		PromptText   string
		HTTPDebug    sessionfs.HTTPDebugSettings
		HTTPDebugDir string
		// The runner holds the retry settings, so a changed setting needs a new runner.
		ProviderRetry sessionfs.ProviderRetrySettings
		// The client holds the idle timeout too.
		ProviderStream sessionfs.ProviderStreamSettings
		// The transport's dialer holds the address family.
		ProviderNetwork sessionfs.ProviderNetworkSettings
		// The runner holds the OAuth transport, so the credential file and the endpoint
		// and margin it was built with identify the runner. The token is not included.
		OAuthAuthFile string
		OpenAIOAuth   sessionfs.OpenAIOAuthSettings
		// The in-process translator is built from the protocol and its settings.
		API             string
		Anthropic       json.RawMessage
		ChatCompletions json.RawMessage
		// The priced wrapper holds the pricing.
		Pricing *sessionfs.ModelPricing
		// The runner's model settings hold the prompt cache key.
		PromptCacheKey string
	}{
		PromptCacheKey:  config.Model.PromptCacheKey,
		ModelPath:       config.ModelPath,
		Provider:        config.Model.Provider,
		Model:           config.Model.Name,
		APIKeyEnv:       config.Model.APIKeyEnv,
		BaseURL:         config.Model.BaseURL,
		AuthMode:        config.Model.AuthMode,
		Request:         config.Model.Request,
		HostedTools:     config.Model.HostedTools,
		PromptRole:      config.Model.PromptRole,
		PromptText:      config.Model.PromptText,
		HTTPDebug:       config.HTTPDebug,
		HTTPDebugDir:    config.StoragePaths.HTTPDebug,
		ProviderRetry:   config.ProviderRetry,
		ProviderStream:  config.ProviderStream,
		ProviderNetwork: config.ProviderNetwork,
		OAuthAuthFile:   config.Model.OAuthAuthFile,
		OpenAIOAuth:     config.OpenAIOAuth,
		API:             config.Model.API,
		Anthropic:       config.Model.Anthropic,
		ChatCompletions: config.Model.ChatCompletions,
		Pricing:         config.Model.Pricing,
	}
	keyHash := sha256.Sum256([]byte(apiKey))
	identity.APIKeyHash = hex.EncodeToString(keyHash[:])
	encoded, _ := json.Marshal(identity)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

// WatchSessionAsync implements httpapi.AsyncStateSource: it follows the session's handles in
// every async manager of the resolver and reads their summed counts and newest live ids.
func (r *Resolver) WatchSessionAsync(reference, profile string, notify func()) (func() httpapi.AsyncState, func()) {
	owner := sessionKey(reference, profile)
	managers := r.asyncManagers()
	if len(managers) == 0 {
		return nil, func() {}
	}
	cancels := make([]func(), 0, len(managers))
	for _, manager := range managers {
		cancels = append(cancels, manager.Watch(owner, notify))
	}
	limit := int(managers[0].Config().SSEMaxIDs)
	for _, manager := range managers {
		limit = min(limit, int(manager.Config().SSEMaxIDs))
	}
	read := func() httpapi.AsyncState {
		state := httpapi.AsyncState{}
		var live []asyncmgr.LiveID
		for _, manager := range managers {
			counts, ids := manager.SnapshotLive(owner)
			state.Active += counts.Active
			state.Unread += counts.Unread
			live = append(live, ids...)
		}
		slices.SortFunc(live, func(a, b asyncmgr.LiveID) int {
			if c := b.Changed.Compare(a.Changed); c != 0 {
				return c
			}
			return strings.Compare(b.ID, a.ID)
		})
		for i := 0; i < len(live) && i < limit; i++ {
			state.IDs = append(state.IDs, live[i].ID)
		}
		return state
	}
	return read, func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
}

// sessionAsyncManager is the async manager of the session's tool storage, or nil when the
// session has no async tool or its host is not open yet.
func (r *Resolver) sessionAsyncManager(config sessionfs.SessionConfig) *asyncmgr.Manager {
	if config.NativeAsync == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if host := r.asyncHosts[filepath.Clean(native.AsyncDir(config.StoragePaths.ToolStorage))]; host != nil {
		return host.Manager()
	}
	return nil
}
