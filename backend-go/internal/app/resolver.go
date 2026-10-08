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
	"strings"
	"sync"

	"example.com/agent-harness/backend-go/internal/checkpoint"
	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/httpdebug"
	"example.com/agent-harness/backend-go/internal/logging"
	"example.com/agent-harness/backend-go/internal/openaiapi"
	"example.com/agent-harness/backend-go/internal/session"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionlock"
	"example.com/agent-harness/backend-go/internal/tools"
	mcpclient "example.com/agent-harness/backend-go/internal/tools/mcp"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
	"example.com/agent-harness/backend-go/internal/webui"
)

// Resolver composes the root catalog, on-disk session configuration, transcript
// stores, and provider clients for the HTTP run routes.
type CatalogConfig struct {
	SnapshotTurns      int
	MaxPageTurns       int
	MaxInlinePartBytes int
}

type Resolver struct {
	logger      *slog.Logger
	skipped     skippedSessions
	roots       []sessionfs.Root
	environment map[string]string
	catalog     CatalogConfig

	mu               sync.Mutex
	settingsMu       sync.Mutex
	stores           map[string]*sharedSession
	processManagers  map[string]*native.ManagedProcessManager
	subagentManagers map[string]*native.SubagentManager
	mcpManagers      map[string]*mcpclient.Manager
	taskStores       map[string]*native.TaskStore
	memoryStores     map[string]*native.MemoryStore
	fetchStores      map[string]*native.FetchStore
	mutationLock     *native.PathLocks
	closed           bool
}

type sharedSession struct {
	store        *transcript.TurnStore
	staging      string
	durability   transcript.DurabilityMode
	options      transcript.TurnStoreOptions
	runMu        sync.Mutex
	turnStartsMu sync.Mutex
	turnStarts   *turnStartsCache
	runners      map[string]*session.Runner
}

// boundRunner is a session resolved from its settings. Its model, tools and
// credential are prepared inside Run, once the user input is durable.
type boundRunner struct {
	resolver       *Resolver
	shared         *sharedSession
	config         sessionfs.SessionConfig
	reference      string
	processManager native.ProcessManager
	taskStore      *native.TaskStore
	memoryStore    *native.MemoryStore
}

// sessionKey names one session and profile, like the HTTP run registry's key. It is the
// owner of the sub-agent runs the session starts, so a stop or reset of the session
// reaches exactly those.
func sessionKey(reference, profile string) string {
	return reference + "\x00" + profile
}

// CancelSessionChildren asks every sub-agent run a session started to stop, and
// reports how many were still going. The HTTP layer calls it when the user stops or
// resets the session: a child handed off earlier would otherwise keep spending after
// the user asked for everything to end. It does not wait for the children.
func (r *Resolver) CancelSessionChildren(reference, profile string) int {
	r.mu.Lock()
	managers := make([]*native.SubagentManager, 0, len(r.subagentManagers))
	for _, manager := range r.subagentManagers {
		managers = append(managers, manager)
	}
	r.mu.Unlock()
	owner := sessionKey(reference, profile)
	cancelled := 0
	for _, manager := range managers {
		cancelled += manager.CancelOwner(owner)
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
	if catalog.SnapshotTurns < 0 || catalog.MaxPageTurns < 1 || catalog.MaxInlinePartBytes < 0 {
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
	return &Resolver{
		logger:           resolverOptions.logger,
		roots:            copyRoots,
		environment:      copyEnvironment,
		catalog:          catalog,
		stores:           make(map[string]*sharedSession),
		processManagers:  make(map[string]*native.ManagedProcessManager),
		subagentManagers: make(map[string]*native.SubagentManager),
		mcpManagers:      make(map[string]*mcpclient.Manager),
		taskStores:       make(map[string]*native.TaskStore),
		memoryStores:     make(map[string]*native.MemoryStore),
		fetchStores:      make(map[string]*native.FetchStore),
		mutationLock:     native.NewPathLocks(),
	}, nil
}

// LockSession implements httpapi.SessionProcessLocker. It locks the session
// settings before resolving storage, then locks the selected transcript. The
// ordering coordinates profile changes and shared transcript paths.
func (r *Resolver) LockSession(ctx context.Context, reference, profile, modelPath string) (func(), error) {
	location, err := r.resolveLockLocation(ctx, reference)
	if err != nil {
		return nil, err
	}
	settingsLock, err := r.acquireSettingLock(ctx, location)
	if err != nil {
		return nil, err
	}
	// Locking needs only the storage paths. The model file is not read until the
	// run itself needs it, so a broken model.json cannot block a run from
	// reaching the point where its input is stored.
	config, err := sessionfs.LoadSessionSettings(location, profile, modelPath)
	if err != nil {
		settingsLock()
		return nil, mapSessionError(err)
	}
	transcriptLockPath, err := sessionlock.PathFor(config.MessagesPath)
	if err != nil {
		settingsLock()
		return nil, err
	}
	transcriptLock, err := sessionlock.Acquire(ctx, transcriptLockPath)
	if err != nil {
		settingsLock()
		return nil, fmt.Errorf("lock session transcript: %w", err)
	}
	return func() {
		_ = transcriptLock.Close()
		settingsLock()
	}, nil
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
	info, err := os.Stat(location.Directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionfs.Location{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "session not found"}
		}
		return sessionfs.Location{}, fmt.Errorf("inspect session directory: %w", err)
	}
	if !info.IsDir() {
		return sessionfs.Location{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "session not found"}
	}
	return location, nil
}

func (r *Resolver) acquireSettingLock(ctx context.Context, location sessionfs.Location) (func(), error) {
	path, err := sessionlock.PathFor(filepath.Join(location.Directory, "setting.json"))
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
	config, err := sessionfs.LoadSessionSettings(location, profile, modelPath)
	if err != nil {
		return httpapi.ResolvedSession{}, mapSessionError(err)
	}
	shared, err := r.sharedStore(config)
	if err != nil {
		return httpapi.ResolvedSession{}, err
	}
	return httpapi.ResolvedSession{
		Reference: location.Reference,
		Profile:   config.Profile,
		Runner:    boundRunner{resolver: r, shared: shared, config: config, reference: location.Reference},
	}, nil
}

// preparedRun is everything a run needs beyond the session settings.
type preparedRun struct {
	config         sessionfs.SessionConfig
	runner         *session.Runner
	toolCatalog    *tools.Catalog
	processManager native.ProcessManager
	taskStore      *native.TaskStore
	memoryStore    *native.MemoryStore
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
	toolCatalog, processManager, taskStore, memoryStore, err := r.buildToolCatalog(ctx, config)
	if err != nil {
		return preparedRun{}, err
	}
	apiKey, err := r.providerAPIKey(config)
	if err != nil {
		return preparedRun{}, err
	}
	runnerKey := fingerprintRunner(config, apiKey)
	runner := shared.runners[runnerKey]
	if runner == nil {
		streamer := r.newProviderStreamer(config, apiKey)
		runner, err = session.NewRunner(shared.store, streamer, session.ModelSettings{
			Model:      config.Model.Name,
			Request:    config.Model.Request,
			PromptRole: config.Model.PromptRole,
			PromptText: config.Model.PromptText,
		})
		if err != nil {
			return preparedRun{}, fmt.Errorf("create session runner: %w", err)
		}
		shared.runners[runnerKey] = runner
	}
	return preparedRun{config: config, runner: runner, toolCatalog: toolCatalog, processManager: processManager, taskStore: taskStore, memoryStore: memoryStore}, nil
}

// providerAPIKey returns the key handed to the provider client for a loaded model, and
// fails the run before it starts when the credential cannot be used.
func (r *Resolver) providerAPIKey(config sessionfs.SessionConfig) (string, error) {
	if config.Model.AuthMode == "openai_oauth" {
		// The token itself is read from the file on every request, so it is not part of
		// the runner's identity; the file path and the OAuth settings are.
		if err := checkOAuthCredential(config); err != nil {
			return "", err
		}
		return oauthPlaceholderKey, nil
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
func (r *Resolver) newProviderStreamer(config sessionfs.SessionConfig, apiKey string) *openaiapi.Client {
	var httpClient *http.Client
	var network http.RoundTripper = http.DefaultTransport
	if config.Model.AuthMode == "openai_oauth" {
		network = oauthTransport(config, network)
		httpClient = &http.Client{Transport: network}
	}
	if config.HTTPDebug.Enabled {
		transport := httpdebug.NewTransport(config.StoragePaths.HTTPDebug, httpdebug.Settings{
			Logger:                 r.logger,
			Enabled:                true,
			IncludeRequestHeaders:  config.HTTPDebug.IncludeRequestHeaders,
			IncludeRequestBody:     config.HTTPDebug.IncludeRequestBody,
			IncludeResponseHeaders: config.HTTPDebug.IncludeResponseHeaders,
			IncludeResponseBody:    config.HTTPDebug.IncludeResponseBody,
			MaxBodyBytes:           config.HTTPDebug.MaxBodyBytes,
		}, network)
		httpClient = &http.Client{Transport: transport}
	}
	retry := providerRetryConfig(config.ProviderRetry, r.logger)
	return openaiapi.NewClient(openaiapi.Config{APIKey: apiKey, BaseURL: config.Model.BaseURL, HTTPClient: httpClient, Retry: &retry})
}

// ListTools implements httpapi.SessionToolLister without requiring a provider
// API key or creating a session runner.
func (r *Resolver) ListTools(ctx context.Context, reference, profile string) ([]tools.Definition, error) {
	if ctx == nil {
		return nil, errors.New("session resolution context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return nil, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return nil, mapSessionError(err)
	}
	catalog, _, _, _, err := r.buildToolCatalog(ctx, config)
	if err != nil {
		return nil, err
	}
	return catalog.Definitions(), nil
}

func (r *Resolver) buildToolCatalog(ctx context.Context, config sessionfs.SessionConfig) (*tools.Catalog, native.ProcessManager, *native.TaskStore, *native.MemoryStore, error) {
	processManager := r.sharedProcessManager(config)
	taskStore := r.sharedTaskStore(config)
	memoryStore := r.sharedMemoryStore(config)
	fetchStore := r.sharedFetchStore(config)
	subagentMCP := &native.SubagentMCP{}
	nativeCatalog, err := native.BuildCatalogWithSubagents(config, r.mutationLock, processManager, taskStore, memoryStore, fetchStore, r.subagentLauncherFor(config), subagentMCP, r.sharedSubagentManager(config))
	if err != nil {
		return nil, nil, nil, nil, mapSessionError(err)
	}
	mcpManager, err := r.sharedMCPManager(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	mcpTools, err := mcpManager.LoadTools(ctx, config.MCPServers, config.WorkingDir)
	if err != nil {
		return nil, nil, nil, nil, mapSessionError(err)
	}
	mcpCatalog, err := tools.NewCatalog(mcpTools)
	if err != nil {
		return nil, nil, nil, nil, mapSessionError(err)
	}
	// The subagent tool was built with the native catalog, before the MCP tools existed.
	if err := subagentMCP.Bind(mcpTools); err != nil {
		return nil, nil, nil, nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)})
	}
	toolCatalog, err := tools.MergeCatalogs(nativeCatalog, mcpCatalog)
	if err != nil {
		return nil, nil, nil, nil, mapSessionError(err)
	}
	return toolCatalog, processManager, taskStore, memoryStore, nil
}

func (r *Resolver) sharedProcessManager(config sessionfs.SessionConfig) native.ProcessManager {
	if config.NativeProcess == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeProcess.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if manager := r.processManagers[storageKey]; manager != nil {
		return manager
	}
	if r.closed {
		return nil
	}
	manager := native.NewProcessManager(storageKey)
	r.processManagers[storageKey] = manager
	return manager
}

// sharedSubagentManager returns the manager that owns the child runs of one storage
// directory, so a run started in one request can be read in a later one.
func (r *Resolver) sharedSubagentManager(config sessionfs.SessionConfig) *native.SubagentManager {
	if config.NativeSubagent == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeSubagent.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if manager := r.subagentManagers[storageKey]; manager != nil {
		return manager
	}
	if r.closed {
		return nil
	}
	var manager *native.SubagentManager
	if config.NativeSubagent.PersistRuns {
		manager = native.NewPersistentSubagentManager(native.SubagentManagerOptions{
			StorageDir: config.NativeSubagent.StorageDir, Keep: int(config.NativeSubagent.KeepFinished), Logger: r.logger,
			DeleteTranscripts: config.NativeSubagent.DeleteTranscripts,
		})
	} else {
		manager = native.NewSubagentManager()
		if config.NativeSubagent.DeleteTranscripts {
			manager.DeleteTranscriptsUnder(config.NativeSubagent.StorageDir)
		}
	}
	r.subagentManagers[storageKey] = manager
	return manager
}

func (r *Resolver) sharedTaskStore(config sessionfs.SessionConfig) *native.TaskStore {
	if config.NativeTask == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeTask.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store := r.taskStores[storageKey]; store != nil {
		return store
	}
	if r.closed {
		return nil
	}
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
		return store
	}
	if r.closed {
		return nil
	}
	store := native.NewMemoryStore(storageKey, r.mutationLock)
	r.memoryStores[storageKey] = store
	return store
}

func (r *Resolver) sharedFetchStore(config sessionfs.SessionConfig) *native.FetchStore {
	if config.NativeFetch == nil {
		return nil
	}
	storageKey := filepath.Clean(config.NativeFetch.StorageDir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if store := r.fetchStores[storageKey]; store != nil {
		return store
	}
	if r.closed {
		return nil
	}
	store := native.NewFetchStore(storageKey)
	r.fetchStores[storageKey] = store
	return store
}

func (r *Resolver) sharedMCPManager(config sessionfs.SessionConfig) (*mcpclient.Manager, error) {
	key := filepath.Clean(config.SettingPath)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	if manager := r.mcpManagers[key]; manager != nil {
		return manager, nil
	}
	manager := mcpclient.NewManager(r.environment)
	r.mcpManagers[key] = manager
	return manager, nil
}

func (r *Resolver) sharedStore(config sessionfs.SessionConfig) (*sharedSession, error) {
	acceptedRequestPath := filepath.Join(config.StagingDir, "accepted-request.json")
	if len(config.PathProfiles) == 0 {
		acceptedRequestPath = filepath.Join(filepath.Dir(config.SettingPath), "accepted-request.json")
	}
	storeOptions := transcript.TurnStoreOptions{
		Logger:                     r.logger,
		MaxItemBytes:               config.Persistence.MaxItemBytes,
		ResponseMetadata:           transcript.ResponseMetadataMode(config.Persistence.ResponseMetadata),
		MessagesBackupPath:         config.StoragePaths.MessagesBackup,
		CompactionCachePath:        config.StoragePaths.CompactionCache,
		HistoryCachePath:           config.StoragePaths.HistoryCache,
		UsageCachePath:             config.StoragePaths.UsageCache,
		CursorCachePath:            config.StoragePaths.CursorCache,
		RunErrorCachePath:          config.StoragePaths.RunErrorCache,
		CursorCacheCheckpointBytes: config.Persistence.CursorCacheCheckpointBytes,
		StateContextPath:           config.StoragePaths.StateContext,
		AcceptedRequestPath:        acceptedRequestPath,
	}
	storageKey := filepath.Clean(config.MessagesPath)
	durability := transcript.DurabilityMode(config.Persistence.Durability)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, &httpapi.StatusError{Code: http.StatusServiceUnavailable, Message: "server is shutting down"}
	}
	shared := r.stores[storageKey]
	if shared == nil {
		if err := os.MkdirAll(filepath.Dir(config.MessagesPath), 0o755); err != nil {
			return nil, fmt.Errorf("create transcript directory: %w", err)
		}
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
		shared = &sharedSession{
			store:      store,
			staging:    filepath.Clean(config.StagingDir),
			durability: durability,
			options:    storeOptions,
			runners:    make(map[string]*session.Runner),
		}
		r.stores[storageKey] = shared
	} else if shared.options != storeOptions || shared.staging != filepath.Clean(config.StagingDir) || shared.durability != durability {
		return nil, &httpapi.StatusError{
			Code:    http.StatusUnprocessableEntity,
			Message: "session path profiles share transcript storage but have incompatible staging or persistence settings",
		}
	}
	return shared, nil
}

// Close prevents new resolutions and stops managed native processes.
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
	managers := make([]*native.ManagedProcessManager, 0, len(r.processManagers))
	for _, manager := range r.processManagers {
		managers = append(managers, manager)
	}
	mcpManagers := make([]*mcpclient.Manager, 0, len(r.mcpManagers))
	for _, manager := range r.mcpManagers {
		mcpManagers = append(mcpManagers, manager)
	}
	subagentManagers := make([]*native.SubagentManager, 0, len(r.subagentManagers))
	for _, manager := range r.subagentManagers {
		subagentManagers = append(subagentManagers, manager)
	}
	r.mu.Unlock()
	// Children first: they may be using MCP tools the next loop closes.
	for _, manager := range subagentManagers {
		_ = manager.Close()
	}
	for _, manager := range managers {
		_ = manager.Close()
	}
	for _, manager := range mcpManagers {
		_ = manager.Close()
	}
}

func (r boundRunner) Run(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
	r.shared.runMu.Lock()
	defer r.shared.runMu.Unlock()
	nextSessionTurn := func() (int64, error) {
		index, err := scanTranscriptIndex(r.shared, r.config)
		if err != nil {
			return 0, fmt.Errorf("read prior usage: %w", err)
		}
		if index.usage.TurnCount >= maxSafeUsageInteger {
			return 0, errors.New("session turn counter is exhausted")
		}
		return index.usage.TurnCount + 1, nil
	}
	// The user input is stored before anything that can still fail (an unreadable
	// model file, an unreachable tool server, a missing credential), exactly as
	// the TypeScript run does: the message is not lost, and the next run repairs
	// and continues from it.
	persisted, err := session.PersistInput(r.shared.store, input, nextSessionTurn)
	if err != nil {
		return session.RunResult{}, err
	}
	if options.OnInputAccepted != nil {
		options.OnInputAccepted(*persisted)
	}
	prepared, err := r.resolver.prepareRun(ctx, r.shared, r.config)
	if err != nil {
		return session.RunResult{}, err
	}
	// From here on the configuration includes the model.
	r.config = prepared.config
	r.processManager, r.taskStore, r.memoryStore = prepared.processManager, prepared.taskStore, prepared.memoryStore
	options.PersistedInput = persisted
	options.ToolCatalog = prepared.toolCatalog
	options.MaxToolRounds = int(r.config.MaxToolRounds)
	options.MaxRunTokens = r.config.MaxRunTokens
	options.MaxParallelToolCalls = int(r.config.MaxParallelToolCalls)
	options.ToolCallScheduling = r.config.ToolCallScheduling
	options.Approval = &tools.ApprovalOptions{
		TimeoutMS:     r.config.Approval.TimeoutMS,
		ExtendMS:      r.config.Approval.ExtendMS,
		MaxTimeoutMS:  r.config.Approval.MaxTimeoutMS,
		HeadlessAllow: r.config.Approval.Headless == sessionfs.ApprovalHeadlessAllow,
		PreviewBytes:  r.config.Approval.PreviewBytes,
	}
	if r.config.Checkpoint.Enabled {
		options.Checkpoint = checkpoint.ForSession(r.config, r.resolver.logger)
	}
	options.ToolExecution.WorkspaceRoot = r.config.WorkingDir
	options.ToolExecution.SessionDir = filepath.Dir(r.config.SettingPath)
	options.ToolExecution.SessionKey = sessionKey(r.reference, r.config.Profile)
	options.ToolExecution.OnInteractionResolved = func(identity tools.InteractionIdentity, answer tools.InteractionAnswer, proposed json.RawMessage) error {
		if answer.Decision != tools.InteractionAccept && answer.Decision != tools.InteractionUpdate {
			return nil
		}
		draft := answer.Draft
		if len(draft) == 0 {
			draft = proposed
		}
		source := string(answer.Source)
		if source == "" {
			source = string(tools.InteractionSourceUser)
		}
		_, err := r.shared.store.WriteAcceptedRequest(draft, source, string(answer.Decision), identity.ToolCallID)
		return err
	}
	options.AcceptedRequestRole = r.config.StateContext.Role
	options.AutomaticCompaction = &session.AutomaticCompactionSettings{
		Enabled:       r.config.Compaction.AutoEnabled,
		TriggerTokens: r.config.Compaction.TriggerTokens,
		TurnType:      r.config.Compaction.TurnType,
		KeepTurns:     r.config.Compaction.KeepTurns,
		KeepToolPairs: r.config.Compaction.KeepToolPairs,
		BackupMode:    r.config.Compaction.BackupMode,
	}
	options.NextSessionTurn = nextSessionTurn
	options.CompactHistory = func(compaction transcript.CompactionOptions) (transcript.CompactionResult, error) {
		return r.shared.compactPreservingUsage(r.config, compaction)
	}
	options.RefreshStateContext = func(context.Context) (*session.StateContextPrompt, error) {
		snapshot, include, err := collectStateContextSnapshot(r.config, stateContextSources{
			process: r.processManager, task: r.taskStore, memory: r.memoryStore,
			subagent: r.resolver.sharedSubagentManager(r.config), owner: sessionKey(r.reference, r.config.Profile),
		}, "compaction")
		if err != nil {
			return nil, fmt.Errorf("collect state context after automatic compaction: %w", err)
		}
		if !include {
			return nil, nil
		}
		if err := r.shared.store.WriteStateContextSnapshot(snapshot); err != nil {
			return nil, fmt.Errorf("persist state context after automatic compaction: %w", err)
		}
		return &session.StateContextPrompt{Role: r.config.StateContext.Role, Text: snapshot.Text}, nil
	}
	options.StateContext = nil
	if snapshot, exists, err := r.shared.store.ReadStateContextSnapshot(); err != nil {
		return session.RunResult{}, fmt.Errorf("read state context snapshot: %w", err)
	} else if exists && r.config.StateContext.Enabled && containsStateContextTrigger(r.config.StateContext.Trigger, snapshot.Trigger) {
		options.StateContext = &session.StateContextPrompt{Role: r.config.StateContext.Role, Text: snapshot.Text}
	}
	return prepared.runner.Run(ctx, input, options)
}

// ReadRunUsageSummary reads the session's usage from inside a run, where the
// run already holds runMu (it serializes against compaction and reset), so it
// must not be called from outside Run callbacks.
func (r boundRunner) ReadRunUsageSummary(ctx context.Context) (webui.UsageSummary, error) {
	var empty webui.UsageSummary
	if ctx == nil {
		return empty, errors.New("usage summary context is required")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if r.shared == nil || r.shared.store == nil || r.config.MessagesPath == "" {
		return empty, errors.New("bound session runner is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	index, err := scanTranscriptIndex(r.shared, r.config)
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return index.usage, nil
}

// RecordRunError keeps a failed run in the session's run-error cache, outside
// messages.jsonl. The cap is the session's persistence.run_error_cache_max_entries.
func (r boundRunner) RecordRunError(entry transcript.RunErrorEntry) error {
	if r.shared == nil || r.shared.store == nil {
		return errors.New("bound session runner is not initialized")
	}
	return r.shared.store.RecordRunError(entry, r.config.Persistence.RunErrorCacheMaxEntries)
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
		PromptRole   string
		PromptText   string
		HTTPDebug    sessionfs.HTTPDebugSettings
		HTTPDebugDir string
		// The runner holds the retry settings, so a changed setting needs a new runner.
		ProviderRetry sessionfs.ProviderRetrySettings
		// The runner holds the OAuth transport, so the credential file and the endpoint
		// and margin it was built with identify the runner. The token is not included.
		OAuthAuthFile string
		OpenAIOAuth   sessionfs.OpenAIOAuthSettings
	}{
		ModelPath:     config.ModelPath,
		Provider:      config.Model.Provider,
		Model:         config.Model.Name,
		APIKeyEnv:     config.Model.APIKeyEnv,
		BaseURL:       config.Model.BaseURL,
		AuthMode:      config.Model.AuthMode,
		Request:       config.Model.Request,
		PromptRole:    config.Model.PromptRole,
		PromptText:    config.Model.PromptText,
		HTTPDebug:     config.HTTPDebug,
		HTTPDebugDir:  config.StoragePaths.HTTPDebug,
		ProviderRetry: config.ProviderRetry,
		OAuthAuthFile: config.Model.OAuthAuthFile,
		OpenAIOAuth:   config.OpenAIOAuth,
	}
	keyHash := sha256.Sum256([]byte(apiKey))
	identity.APIKeyHash = hex.EncodeToString(keyHash[:])
	encoded, _ := json.Marshal(identity)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
