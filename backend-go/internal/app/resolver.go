package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// callerRuns tracks the targets each running caller run has compacted; callerRunSeq
	// hands out the run ids.
	callerRuns callerRunTargets
	// profileQueues holds the profile_message calls waiting for a busy target profile.
	profileQueues profileQueueSet
	callerRunSeq  atomic.Uint64
	// compactProfile replaces compactTarget in tests.
	compactProfile func(ctx context.Context, reference, profile string) error
	// profileUsage replaces profileMessenger.ProfileInfo in tests.
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
	taskStores    map[string]*native.TaskStore
	memoryStores  map[string]*native.MemoryStore
	// storeSessions lists the session files that use each task and memory store,
	// keyed by kind and storage key, so deleting the last of them can release the store.
	storeSessions map[string]map[string]struct{}
	mutationLock  *native.PathLocks
	closed        bool

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
// session's handles of every kind: processes and sub-agent runs) to stop, and reports how
// many were still going. The HTTP layer calls it when the user stops the session. It does
// not wait. A normal end of a run or a compaction never comes here.
func (r *Resolver) CancelSessionChildren(reference, profile string) int {
	owner := sessionKey(reference, profile)
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.CancelOwner(owner, reasonUserStop)
	}
	return cancelled + r.cancelSubagentOwner(owner, reasonUserStop)
}

// ResetSessionChildren is CancelSessionChildren for a reset: the session's unread
// completion signals are dropped too, because the history that gave them meaning is gone.
func (r *Resolver) ResetSessionChildren(reference, profile string) int {
	owner := sessionKey(reference, profile)
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.ResetOwner(owner, reasonSessionReset)
	}
	return cancelled + r.cancelSubagentOwner(owner, reasonSessionReset)
}

// BackgroundWriters implements httpapi.SessionWriters: how many sub-agent runs of the session
// can change files and are going as handles, in every sub-agent manager of the resolver.
func (r *Resolver) BackgroundWriters(reference, profile string) int {
	owner := sessionKey(reference, profile)
	count := 0
	for _, manager := range r.subagentManagerList() {
		count += manager.BackgroundWriters(owner)
	}
	return count
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
		memoryStores:      make(map[string]*native.MemoryStore),
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

// preparedRun is everything a run needs beyond the session settings.
type preparedRun struct {
	config      sessionfs.SessionConfig
	runner      *session.Runner
	toolCatalog *tools.Catalog
	taskStore   *native.TaskStore
	memoryStore *native.MemoryStore
}

// prepareRun reads the model file, builds the tool catalog (connecting MCP
// servers), resolves the credential and returns the shared runner. The steps and
// their order follow the TypeScript run: model, then tools, then the client.
// The caller holds shared.runMu, which also guards shared.runners.
func (r *Resolver) prepareRun(ctx context.Context, shared *sharedSession, settings sessionfs.SessionConfig) (preparedRun, error) {
	config, err := settings.WithModel()
	if err != nil {
		return preparedRun{}, err
	}
	toolCatalog, taskStore, memoryStore, err := r.buildToolCatalog(ctx, config)
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
	return preparedRun{config: config, runner: runner, toolCatalog: toolCatalog, taskStore: taskStore, memoryStore: memoryStore}, nil
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
	registered, _, _, err := r.buildRegisteredCatalog(ctx, config)
	if err != nil {
		return httpapi.SessionTools{}, err
	}
	exposed, hidden, err := applyProfileTools(config, registered)
	if err != nil {
		return httpapi.SessionTools{}, mapSessionError(err)
	}
	return httpapi.SessionTools{Tools: exposed.ListedDefinitions(), Hidden: hidden}, nil
}

// buildToolCatalog is the catalog the profile's model sees: every registered tool, narrowed
// by the profile's tools.allow when it has one.
func (r *Resolver) buildToolCatalog(ctx context.Context, config sessionfs.SessionConfig) (*tools.Catalog, *native.TaskStore, *native.MemoryStore, error) {
	registered, taskStore, memoryStore, err := r.buildRegisteredCatalog(ctx, config)
	if err != nil {
		return nil, nil, nil, err
	}
	// The profile narrows what its own model sees, not what a child may use: the
	// sub-agent tool built its agents' catalogs from the registered tools already.
	toolCatalog, _, err := applyProfileTools(config, registered)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	return toolCatalog, taskStore, memoryStore, nil
}

// buildRegisteredCatalog builds every tool the session registers (native, then MCP).
func (r *Resolver) buildRegisteredCatalog(ctx context.Context, config sessionfs.SessionConfig) (*tools.Catalog, *native.TaskStore, *native.MemoryStore, error) {
	taskStore := r.sharedTaskStore(config)
	memoryStore := r.sharedMemoryStore(config)
	subagentMCP := native.NewSubagentMCP(config.Hooks, config.Model.APIKeyEnv)
	// One lazy set per prepare: read from the state folder once, and passed to both the native
	// pass and the MCP pass below. Nil when the profile has no tools.lazy.
	lazy, err := openLazySet(config, r.logger)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	nativeCatalog, err := native.BuildCatalogWithAsync(config, r.mutationLock, taskStore, memoryStore, r.subagentLauncherFor(config), subagentMCP, r.sharedSubagentManager(config), r, profileMessenger{resolver: r}, r, lazy)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	mcpManager, err := r.sharedMCPManager(config)
	if err != nil {
		return nil, nil, nil, err
	}
	// A server whose file enables sampling is answered with this run's model.
	mcpManager.SetSampler(r.mcpSampler(config))
	mcpManager.SetCommandWrapper(mcpSandboxWrapper(config))
	mcpTools, err := mcpManager.LoadTools(ctx, config.MCPServers, config.WorkingDir)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	// The parent's copy is gated (its lazy tools only) and hooked. The subagent's children get the
	// raw list: each child's session copy is gated with the same set and hooked by the subagent tool
	// (bindMCP, GateFor), and a toolset's copy is hooked with its own profile's hooks.
	lazy.Bind(mcpTools)
	parentMCPTools := native.WithToolEventHooksAll(native.GateLazyTools(mcpTools, lazy), config.Hooks, config.Model.APIKeyEnv)
	mcpCatalog, err := tools.NewCatalog(parentMCPTools)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	toolsetMCP, err := r.loadAgentToolsetMCP(ctx, config)
	if err != nil {
		return nil, nil, nil, err
	}
	// A child gates its MCP tools with the owning profile's set and gets the owner's load_tool when
	// the owner enables it. Both come from the owner's config, so a child reads the same state folder.
	var childLoadTool *tools.Tool
	if lazy != nil && config.NativeLoadTool != nil {
		tool, err := native.ChildLoadTool(config, lazy)
		if err != nil {
			return nil, nil, nil, mapSessionError(err)
		}
		childLoadTool = &tool
	}
	if err := subagentMCP.SetLazy(lazy, childLoadTool); err != nil {
		return nil, nil, nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)})
	}
	// The subagent tool was built with the native catalog, before the MCP tools existed.
	if err := subagentMCP.BindToolsets(mcpTools, toolsetMCP); err != nil {
		return nil, nil, nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)})
	}
	registered, err := tools.MergeCatalogs(nativeCatalog, mcpCatalog)
	if err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	if err := checkHookMatches(config, registered.Definitions(), func(message string) {
		if r.logger != nil {
			r.logger.Warn(message, "profile", config.Profile)
		}
	}); err != nil {
		return nil, nil, nil, mapSessionError(err)
	}
	return registered, taskStore, memoryStore, nil
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
			DeleteTranscripts: config.NativeSubagent.DeleteTranscripts,
		})
	} else {
		manager = native.NewSubagentManager()
		if config.NativeSubagent.DeleteTranscripts {
			manager.DeleteTranscriptsUnder(config.NativeSubagent.StorageDir)
		}
	}
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

func (r *Resolver) sharedMemoryStore(config sessionfs.SessionConfig) *native.MemoryStore {
	if config.NativeMemory == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeMemory.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store := r.memoryStores[storageKey]; store != nil {
		r.noteStoreSession("memory", storageKey, config.SettingPath)
		return store
	}
	if r.closed {
		return nil
	}
	r.noteStoreSession("memory", storageKey, config.SettingPath)
	store := native.NewMemoryStore(storageKey, r.mutationLock)
	r.memoryStores[storageKey] = store
	return store
}

// noteStoreSession records that a session file uses a task or memory store. The
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

// Resources implements native.ToolsetResources: the shared stores of a toolset that a
// sub-agent names, keyed by that toolset's own storage directories.
func (r *Resolver) Resources(config sessionfs.SessionConfig) (*native.TaskStore, *native.MemoryStore) {
	return r.sharedTaskStore(config), r.sharedMemoryStore(config)
}

// loadAgentToolsetMCP connects the MCP servers of every toolset a sub-agent names and
// returns their tools by toolset ID. Each toolset has its own manager, so a server that
// two toolsets use differently is not closed by the other's run.
func (r *Resolver) loadAgentToolsetMCP(ctx context.Context, config sessionfs.SessionConfig) (map[string][]tools.Tool, error) {
	if len(config.AgentToolsets) == 0 {
		return nil, nil
	}
	loaded := make(map[string][]tools.Tool, len(config.AgentToolsets))
	for _, id := range slices.Sorted(maps.Keys(config.AgentToolsets)) {
		derived := config.AgentToolsets[id]
		manager, err := r.mcpManagerFor(filepath.Clean(config.SettingPath) + "\x00" + config.Profile + "\x00toolset\x00" + id)
		if err != nil {
			return nil, err
		}
		mcpTools, err := manager.LoadTools(ctx, derived.MCPServers, derived.WorkingDir)
		if err != nil {
			return nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: toolset %s: %v", config.SettingPath, id, err)})
		}
		loaded[id] = native.WithToolEventHooksAll(mcpTools, derived.Hooks, config.Model.APIKeyEnv)
	}
	return loaded, nil
}

func (r *Resolver) mcpManagerFor(key string) (*mcpclient.Manager, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	if manager := r.mcpManagers[key]; manager != nil {
		return manager, nil
	}
	manager := mcpclient.NewManager(r.environment)
	manager.SetLogger(r.logger)
	r.mcpManagers[key] = manager
	return manager, nil
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
