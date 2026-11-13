package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	profileNamePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	defaultMaxItemBytes                     int64 = 10 * 1024 * 1024
	maxConfiguredItemBytes                  int64 = 64 * 1024 * 1024
	defaultCursorCacheCheckpointBytes       int64 = 1 * 1024 * 1024
	maxConfiguredCursorCacheCheckpointBytes int64 = 64 * 1024 * 1024
	defaultRunErrorCacheMaxEntries                = 50
	maxConfiguredRunErrorCacheMaxEntries          = 10_000
	defaultAutoCompactionEnabled                  = true
	defaultCompactionTriggerTokens          int64 = 200_000
	maxCompactionTriggerTokens              int64 = 10_000_000
	defaultKeepBackupTurns                        = 20
	maxKeepBackupTurns                            = 1000
)

// ConfigError describes invalid on-disk session configuration. The HTTP
// adapter maps these errors to the same unprocessable-configuration response
// used by the TypeScript backend.
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// ClientError represents a validly parsed request that selects an unavailable
// session profile or model path.
type ClientError struct {
	Message string
}

func (e *ClientError) Error() string { return e.Message }

// Persistence contains the subset of per-session persistence settings used by
// the current Go transcript runner.
type Persistence struct {
	Durability                 string
	MaxItemBytes               int64
	ResponseMetadata           string
	CursorCacheCheckpointBytes int64
	// RunErrorCacheMaxEntries caps the failed-run records kept outside the transcript.
	RunErrorCacheMaxEntries int
}

// Compaction contains the normalized settings used by the session compact action.
type Compaction struct {
	AutoEnabled     bool
	TriggerTokens   int64
	TurnType        string
	KeepTurns       int
	KeepToolPairs   int
	BackupMode      string
	KeepBackupTurns int
	// AppendLimit is compaction.keepbackup.append_limit: with backup_mode append, the backup keeps only the newest this many user turns after each compaction; 0 keeps every turn.
	AppendLimit int
	// Overflow is compaction.overflow (off by default; needs auto_enabled).
	Overflow CompactionOverflow
}

type FrontCompactionSettings struct {
	Enabled    bool
	DeleteMode string
}

// Model contains the model settings of one model file.
type Model struct {
	Provider  string
	Name      string
	APIKeyEnv string
	BaseURL   string
	Request   json.RawMessage
	// Efforts are the reasoning efforts a run may choose with "<model>@<effort>"; empty
	// means the model has no choice. Effort is the one applied to Request, "" when none.
	Efforts []string
	Effort  string
	// HostedTools are provider-side tool definitions (for example {"type":"web_search"})
	// the request carries after the function tools. They have no tool code here: the
	// provider runs them. Empty means none.
	HostedTools []json.RawMessage
	PromptRole  string
	PromptText  string
	// API is the protocol of the provider: "responses" (the default), "anthropic_messages"
	// or "chat_completions". The last two are translated in process.
	API string
	// Anthropic is the anthropic object of the file (adapter settings) for API
	// anthropic_messages, and ChatCompletions the chat_completions object for API
	// chat_completions; both are validated at load time and nil when absent.
	Anthropic       json.RawMessage
	ChatCompletions json.RawMessage
	// Pricing is what the model costs per million tokens; nil means unknown, and its runs
	// report no cost.
	Pricing *ModelPricing
	// PromptCacheKey is PromptCacheKeyOff (the default) or PromptCacheKeySession: every
	// request of a session then carries the same prompt_cache_key, derived from the session.
	PromptCacheKey string
}

// Values of a model file's prompt_cache_key.
const (
	PromptCacheKeyOff     = "off"
	PromptCacheKeySession = "session"
)

// Provider protocols a model file can name in api.
const (
	APIResponses         = "responses"
	APIAnthropicMessages = "anthropic_messages"
	APIChatCompletions   = "chat_completions"
)

// RemoteKey is a key file (G/auth/keys/<name>.json) a session may use for a binding.
type RemoteKey struct{ Name, File string }

// SessionConfig is the resolved filesystem-backed configuration for one
// session/profile/model selection.
type SessionStoragePaths struct {
	Messages        string
	MessagesBackup  string
	CompactionCache string
	UsageCache      string
	CursorCache     string
	RunErrorCache   string
	StateContext    string
	Staging         string
	HTTPDebug       string
	ToolStorage     string
}

type SessionConfig struct {
	SessionName    string
	Profile        string
	DefaultProfile string
	Profiles       []string
	// ProfileNames maps a profile ID to the display name its file sets; a profile without a name is absent.
	ProfileNames map[string]string
	ModelPath    string
	// ModelPaths maps each model ID the profile may use to its file. A model whose file fails
	// the model rules is not in it, nor in Models.
	ModelPaths map[string]string
	// ModelBases are the selected profile's own model files (workspace layout, phase 3c) with
	// the model each is based on, by file: a run composes such a file with its base
	// (loadModelOf). Every other model path, a sub-agent's too, is read as a whole model, and
	// an own file read that way fails. nil without own models.
	ModelBases map[string]ModelBase
	// WorkspaceLayout is set for a session of the workspace layout (v2). There a model file read
	// as a whole model may not set based_on; v1 ignores the key (loadWholeModel).
	WorkspaceLayout bool
	// ModelFault is why the model this load would run cannot be used, and then ModelPath is
	// empty (a copy that is given another ModelPath, a sub-agent's or a fallback's, is not
	// affected): the profile's explicit model_default, or the model the caller requested, fails
	// the model rules (the error names its file), or no model of the profile passes. The
	// session still loads, so it can be listed and read; a run (WithModel) stops with this
	// error, and no other model is run in its place.
	ModelFault error
	// ProfilePath is the selected profile's own file in a catalog-layout session; empty otherwise.
	ProfilePath string
	// ProfileOwnDir and ProfileDataDir are the selected profile's private folder and working
	// data folder in the workspace layout (Layout.ProfileOwnDir, ProfileDataDir); empty in v1.
	ProfileOwnDir  string
	ProfileDataDir string
	// ProfileSelfEdit is the profile_self_edit level of the workspace (off, on, full); empty in v1.
	ProfileSelfEdit string
	// SelfEditPaths are the folders profile self-edit writes, which change only through the
	// profile tool (its lock, history and revert): the profile-own folder of every known
	// workspace and this workspace's profile catalog (each profile file is edited by its own
	// profile). Checkpoints leave them out and a restore never writes them (3b.2). Empty in v1.
	SelfEditPaths []string
	// SettingSources are the setting layers of a catalog-layout session and the settings they
	// composed to, for the profile tool's config action; nil otherwise.
	SettingSources *SettingSources
	// ReasoningEffort is the effort chosen with "@<effort>" on the requested model; WithModel
	// applies it to the model's request.
	ReasoningEffort string
	ModelDefault    string
	Models          []string
	SettingPath     string
	// StateDir is <state>/<session id>: the folder the session keeps its state in, and the
	// "session directory" of relative image_store.dir and the {session_dir} spill placeholder.
	StateDir           string
	MessagesPath       string
	MessagesBackupPath string
	UsageCachePath     string
	StagingDir         string
	StoragePaths       SessionStoragePaths
	WorkingDir         string
	SessionFiles       SessionFileListingSettings
	// FilePolicy is session_files.paths: the path rules of the UI file list and of the
	// transcript, transcripts and profile tools.
	FilePolicy               NativePathPolicy
	NativeExec               *NativeExecSettings
	NativeReadImage          *NativeReadImageSettings
	NativeLSP                *NativeLSPSettings
	ExtensionWebSearch       *ExtensionWebSearchSettings
	NativeAsync              *NativeAsyncSettings
	NativeTask               *NativeTaskSettings
	NativeCodeContext        *NativeCodeContextSettings
	NativeRecords            *NativeRecordsSettings
	NativeRequestDraft       *NativeRequestDraftSettings
	NativeSkill              *NativeSkillSettings
	ExtensionDecideSystemone *ExtensionDecideSystemoneSettings
	NativeSubagent           *NativeSubagentSettings
	NativeProfileMessage     *NativeProfileMessageSettings
	NativeToolSmith          *NativeToolSmithSettings
	NativeTranscript         *NativeTranscriptSettings
	NativeTranscripts        *NativeTranscriptsSettings
	NativeProfile            *NativeProfileSettings
	NativeLoadTool           *NativeLoadToolSettings
	// RemotesDir is the remotes folder of the catalog layout (<name>.json entries of other
	// JAD servers); empty outside the layout.
	RemotesDir string
	// RemoteKeys are the session's effective remote keys by binding name (workspace layout):
	// the key files remote_keys of the workspace, the session and the profile resolved to, the
	// nearest level that sets any replacing the ones above. They give a binding its credential
	// and nothing else: what a tool may reach is its own remotes list. nil without keys, and in v1.
	RemoteKeys map[string]RemoteKey
	// RemoteKeyFault is why the session's remote_keys cannot be used (a key file that is missing
	// or invalid, a binding that does not exist, two keys for one binding): RemoteKeys is then
	// empty, and a run (WithModel) stops with this error.
	RemoteKeyFault error
	// AgentToolsets holds, by key, the merged settings of the toolsets a sub-agent names
	// in its toolset key. The key is the ids joined with "+" (one id: the id itself). Only the tool settings and MCP servers of an
	// entry are used; it has no subagent tool.
	AgentToolsets map[string]SessionConfig
	// Warnings are load-time notices, not errors (a tool, server or hooks that two of
	// the toolsets name, where the first listed won). The resolver logs them.
	Warnings []string
	// GlobalJadRoot is the global root whose setting.json names the skill
	// directories the skill tool reads (v1).
	GlobalJadRoot string
	// Libraries are the content libraries of the session's workspace (workspace layout only;
	// nil in v1, where GlobalJadRoot's setting.json names them).
	Libraries           *Libraries
	MCPServers          []MCPServerReference
	StateContext        StateContextSettings
	HTTPDebug           HTTPDebugSettings
	ToolDefinitionPaths []string
	MaxToolRounds       int64
	// MaxParallelToolCalls is the most tool calls of one provider response that
	// run at the same time.
	MaxParallelToolCalls int64
	// ToolCallScheduling is "ordered" or "concurrent": whether a response holding a
	// call that can change state runs its calls one at a time in the model's order.
	ToolCallScheduling string
	// ProviderRetry controls how a failed request to the model provider is retried.
	ProviderRetry ProviderRetrySettings
	// ProviderStream bounds a response stream that has stopped sending.
	ProviderStream ProviderStreamSettings
	// ProviderNetwork chooses the address family of connections to the provider.
	ProviderNetwork ProviderNetworkSettings
	// MaxRunTokens is the most tokens one run may use before it starts another
	// provider round; zero means no cap (the key is absent).
	MaxRunTokens int64
	// ModelFallback lists the models a request goes to when the session's model fails
	// it; nil means none.
	ModelFallback *ModelFallbackSettings
	// Sandbox is the sandbox setting when enabled: the tools it covers carry it in their
	// own settings too. Nil runs every child without one, which is the default.
	Sandbox *SandboxSettings
	// Isolation is sandbox.isolation, kept whether or not the sandbox is enabled: a run that
	// a profile_message starts in this profile works in a git worktree of its own when its
	// mode is worktree (docs/isolation-sandbox-design.md §9.7). The profile's own runs never do
	// (D7).
	Isolation SandboxIsolation `json:",omitzero"`
	// Budget caps what all the runs of the session use, in total and per day.
	Budget BudgetSettings
	// ProjectInstructions finds instruction files such as AGENTS.md by name; off by default.
	ProjectInstructions ProjectInstructionsSettings
	// MaxRunCostUSD is the most US dollars one run may cost, from the models' pricing,
	// before it starts another provider round; zero means no cap.
	MaxRunCostUSD float64
	// MaxRunDurationMS is the most milliseconds one run may take, counted from when it
	// holds the session's run lock; zero means no limit (the key is absent).
	MaxRunDurationMS int64
	// Checkpoint controls run checkpoints and undo; it is off by default.
	Checkpoint CheckpointSettings
	// PlanMode shapes a run started in plan mode.
	PlanMode PlanModeSettings
	// ProfileTools is the selected profile's optional tool allow-list; without it the
	// model sees every registered tool (Go only).
	ProfileTools ProfileToolSettings
	// Hooks holds the optional checks that run after write and edit; off by default (Go only).
	Hooks       HooksSettings
	ImageResize ImageResizeSettings
	// ImageStore keeps large tool-result images in files; off unless a directory is set (Go only).
	ImageStore            ImageStoreSettings
	BackupTurns           int
	Compaction            Compaction
	Persistence           Persistence
	FrontCompaction       json.RawMessage
	FrontCompactionConfig FrontCompactionSettings
	Model                 Model
	// PromptDocuments are the absolute paths of the files read into Model.PromptText.
	PromptDocuments []string
	// ProjectInstructionFiles are the instruction files project_instructions found at
	// and above cwd and added to Model.PromptText after the prompt documents.
	ProjectInstructionFiles []string
}

// LoadSessionSettings reads and validates a catalog-layout session (session.json plus the
// profile, model and toolset files it names) and selects, but does not read, the model file
// (SessionConfig.ModelPath).
// RequestedModel is the model config path from the run body. Callers that need
// the provider settings call WithModel afterwards; the TypeScript backend also
// defers the model file until a run needs it, which is why a session with a
// broken model.json still lists, snapshots and accepts a user message.
func LoadSessionSettings(location Location, requestedProfile, requestedModel string) (SessionConfig, error) {
	return LoadSessionSettingsScoped(location, requestedProfile, requestedModel, nil)
}

// LoadSessionSettingsScoped is LoadSessionSettings that reads each catalog directory
// once per scans, however many sessions load through it. A nil scans reads every time.
func LoadSessionSettingsScoped(location Location, requestedProfile, requestedModel string, scans *CatalogScans) (SessionConfig, error) {
	if location.UsesLayout() {
		return loadLayoutSessionSettings(location, *location.Root.Layout, requestedProfile, requestedModel, scans)
	}
	return SessionConfig{}, configError("%s: not a catalog-layout session; it needs a <session id>.json file in a session root and a root setting.json that names the catalogs", location.File)
}

// loadSessionSettingsFrom validates a setting-shaped map and turns it into a
// SessionConfig. settingPath names the file the map came from in error messages and
// is the base for relative storage paths. The layout loader (layout_load.go) feeds
// it a map composed from the catalogs, so both layouts share every normalizer.
// envFileBase is the folder a relative tools.native.*.env_file is
// resolved against: the session folder for a legacy session, the global root for a
// catalog-layout session, whose toolset many sessions share, and W/.jad in the workspace
// layout (Root.pathBase; a global toolset's relative paths are made absolute from G first).
func loadSessionSettingsFrom(location Location, setting map[string]json.RawMessage, settingPath, envFileBase, requestedProfile, requestedModel string) (SessionConfig, error) {
	if err := validateSchemaVersion(setting, settingPath); err != nil {
		return SessionConfig{}, err
	}
	if err := validateToolSettingsShape(setting, settingPath); err != nil {
		return SessionConfig{}, err
	}
	sessionName, err := stringField(setting, "session_name", location.SessionID, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}

	profile, profileSettings, err := selectProfile(setting, requestedProfile, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	paths, err := objectField(setting, "paths", settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	defaultProfile := stringFieldOr(paths, "active_profile", "")
	pathProfiles, err := orderedObjectKeys(paths["profiles"])
	if err != nil {
		return SessionConfig{}, configError("%s: paths.profiles must be a JSON object", settingPath)
	}
	profileStorageDir := selectProfileStorageDir(location.StateDir(), profileSettings, profile, defaultProfile)
	settingDir := filepath.Dir(settingPath)
	if err := validateAllStoragePaths(location, settingPath, paths, pathProfiles, defaultProfile); err != nil {
		return SessionConfig{}, err
	}
	storagePaths, err := resolveStoragePaths(profileSettings, profileStorageDir, settingDir)
	if err != nil {
		return SessionConfig{}, err
	}
	messagesPath := storagePaths.Messages
	messagesBackupPath := storagePaths.MessagesBackup
	usageCachePath := storagePaths.UsageCache
	stagingDir := storagePaths.Staging
	compactionSettings, err := normalizeCompaction(setting["compaction"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	backupTurns := compactionSettings.KeepBackupTurns

	persistence, err := normalizePersistence(setting["persistence"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	cwd, err := sessionCWD(setting, location.Root.GlobalJadRoot, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	toolStorageDir, err := normalizeToolStorageDir(profileSettings, profile, cwd, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	storagePaths.ToolStorage = toolStorageDir
	sessionFiles, err := normalizeSessionFileListing(setting["session_files"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxToolRounds, err := normalizeMaxToolRounds(setting["max_tool_rounds"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxParallelToolCalls, err := normalizeMaxParallelToolCalls(setting["max_parallel_tool_calls"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	toolCallScheduling, err := normalizeToolCallScheduling(setting["tool_call_scheduling"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	providerRetry, err := normalizeProviderRetrySettings(setting["provider_retry"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	providerStream, err := normalizeProviderStreamSettings(setting["provider_stream"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	providerNetwork, err := normalizeProviderNetworkSettings(setting["provider_network"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxRunTokens, err := normalizeMaxRunTokens(setting["max_run_tokens"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	modelFallback, err := normalizeModelFallback(setting["model_fallback"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxRunCost, err := normalizeMaxRunCost(setting["max_run_cost_usd"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxRunDuration, err := normalizeMaxRunDuration(setting["max_run_duration_ms"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	sandbox, isolation, sandboxObject, err := normalizeSessionSandbox(setting["sandbox"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	budget, err := normalizeBudget(setting["budget"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	projectInstructions, err := normalizeProjectInstructions(setting["project_instructions"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	if err := rejectRemovedApproval(setting, "setting", settingPath); err != nil {
		return SessionConfig{}, err
	}
	checkpoint, err := normalizeCheckpointSettings(setting["checkpoint"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	hooks, err := normalizeHooksSettings(setting["hooks"], settingPath, envFileBase)
	if err != nil {
		return SessionConfig{}, err
	}
	planMode, err := normalizePlanModeSettings(setting["plan_mode"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	var profileTools ProfileToolSettings
	if raw, ok := profileSettings["tools"]; ok {
		if profileTools, err = normalizeProfileTools(raw, settingPath+": paths.profiles."+profile); err != nil {
			return SessionConfig{}, err
		}
	}
	imageResize, err := normalizeImageResizeSettings(setting["image_resize"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	imageStore, err := normalizeImageStoreSettings(setting["image_store"], location.StateDir(), settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	toolDefinitionPaths, err := normalizeToolDefinitionPaths(setting["tool_definition_paths"], location.Root.pathBase(), settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	mcpServers, err := normalizeMCPServers(setting["mcp_servers"], cwd, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	filePolicy, err := normalizeSessionFilesPolicy(setting["session_files"], location.Root.pathBase(), settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeReadImage, err := normalizeNativeReadImageSettings(setting, location.Root.pathBase(), settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeLSP, err := normalizeNativeLSPSettings(setting, location.Root.pathBase(), settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	extensionWebSearch, err := normalizeExtensionWebSearchSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeExec, err := normalizeNativeExecSettings(setting, location.Root.pathBase(), settingPath, cwd, location.StateDir(), storagePaths.ToolStorage, envFileBase)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeTask, err := normalizeNativeTaskSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeCodeContext, err := normalizeNativeCodeContextSettings(setting, location.Root.pathBase(), settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeRecords, err := normalizeNativeRecordsSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeRequestDraft, err := normalizeNativeRequestDraftSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeSkill, err := normalizeNativeSkillSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	extensionDecideSystemone, err := normalizeExtensionDecideSystemoneSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeSubagent, err := normalizeNativeSubagentSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeProfileMessage, err := normalizeNativeProfileMessageSettings(setting, settingPath, messageCallerOf(location))
	if err != nil {
		return SessionConfig{}, err
	}
	nativeToolSmith, err := normalizeNativeToolSmithSettings(setting, location.Root.pathBase(), settingPath, cwd, location.StateDir(), storagePaths.ToolStorage, envFileBase)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeTranscript, err := normalizeNativeTranscriptSettings(setting, settingPath, filePolicy)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeTranscripts, err := normalizeNativeTranscriptsSettings(setting, settingPath, filePolicy)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeProfile, err := normalizeNativeProfileSettings(setting, settingPath, filePolicy)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeLoadTool, err := normalizeNativeLoadToolSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	if err := checkLazyNeedsLoadTool(settingPath, profile, profileTools, nativeLoadTool); err != nil {
		return SessionConfig{}, err
	}
	applySandbox(sandbox, sandboxObject, nativeExec, nativeToolSmith, &hooks, nativeLSP)
	nativeAsync, err := normalizeNativeAsyncSettings(setting, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	if nativeAsync == nil {
		for name, enabled := range map[string]bool{"subagent": nativeSubagent != nil, "tool_smith": nativeToolSmith != nil} {
			if enabled {
				return SessionConfig{}, configError("%s: tools.native.%s needs tools.native.async to collect, list and stop its handles", settingPath, name)
			}
		}
	}
	stateContext, err := normalizeStateContextSettings(setting, settingPath, cwd, nativeTask, nativeRecords, nativeSubagent, nativeAsync)
	if err != nil {
		return SessionConfig{}, err
	}
	httpDebug, err := normalizeHTTPDebugSettings(setting["http_debug"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	workspaceDir := ""
	if location.Root.Layout != nil {
		workspaceDir = location.Root.Layout.Workspace
	}
	promptRole, promptText, promptDocuments, err := loadPrompt(setting, profileSettings, cwd, workspaceDir, settingPath, location.documents)
	if err != nil {
		return SessionConfig{}, err
	}
	instructionText, instructionFiles, err := discoverProjectInstructions(projectInstructions, cwd, promptDocuments, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	if instructionText != "" {
		promptText = strings.Join(slices.DeleteFunc([]string{promptText, instructionText}, func(text string) bool { return text == "" }), "\n\n")
	}

	modelPathValue := "model.json"
	modelDefault := ""
	allowedModels, hasAllowedModels := []string(nil), false
	hasModelDefault := false
	defaultEffort := ""
	if raw, ok := profileSettings["models"]; ok {
		if err := json.Unmarshal(raw, &allowedModels); err != nil || allowedModels == nil {
			return SessionConfig{}, configError("%s: paths.profiles.%s.models must be an array", settingPath, profile)
		}
		for index, modelPath := range allowedModels {
			if strings.TrimSpace(modelPath) == "" {
				return SessionConfig{}, configError("%s: paths.profiles.%s.models[%d] must be a non-empty string", settingPath, profile, index)
			}
			allowedModels[index] = strings.TrimSpace(modelPath)
		}
		hasAllowedModels = true
	}
	if raw, ok := profileSettings["model_default"]; ok {
		var configured string
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return SessionConfig{}, configError("%s: paths.profiles.%s.model_default must be a non-empty string", settingPath, profile)
		}
		modelDefault = strings.TrimSpace(configured)
		if modelPathValue, defaultEffort, err = SplitModelEffort(modelDefault); err != nil {
			return SessionConfig{}, configError("%s: paths.profiles.%s.model_default: %v", settingPath, profile, err)
		}
		hasModelDefault = true
		if !hasAllowedModels {
			allowedModels = []string{modelPathValue}
			hasAllowedModels = true
		}
	} else if len(allowedModels) > 0 {
		modelPathValue = allowedModels[0]
		modelDefault = modelPathValue
		hasModelDefault = true
	}
	if hasModelDefault && hasAllowedModels && !containsString(allowedModels, modelPathValue) {
		return SessionConfig{}, configError("%s: paths.profiles.%s.model_default must be included in models", settingPath, profile)
	}
	reasoningEffort := defaultEffort
	if requestedModel != "" {
		requestedID, requestedEffort := requestedModel, ""
		if !containsString(allowedModels, requestedModel) {
			if requestedID, requestedEffort, err = SplitModelEffort(requestedModel); err != nil {
				return SessionConfig{}, err
			}
		}
		if !hasAllowedModels || !containsString(allowedModels, requestedID) {
			return SessionConfig{}, &ClientError{Message: "model config path is not available in the active session path profile"}
		}
		if requestedEffort != "" || requestedID != modelPathValue {
			reasoningEffort = requestedEffort
		}
		modelPathValue = requestedID
	}
	resolvedModelPath := resolveFrom(settingDir, expandHome(modelPathValue))
	var models []string
	if hasAllowedModels {
		models = append([]string{}, allowedModels...)
	}
	frontRaw, frontExists := setting["front_compaction"]
	frontCompaction, frontCompactionConfig, err := normalizeFrontCompaction(frontRaw, frontExists, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	config := SessionConfig{
		SessionName:              sessionName,
		Profile:                  profile,
		DefaultProfile:           defaultProfile,
		Profiles:                 pathProfiles,
		ModelPath:                resolvedModelPath,
		ReasoningEffort:          reasoningEffort,
		ModelDefault:             modelDefault,
		Models:                   models,
		SettingPath:              settingPath,
		StateDir:                 location.StateDir(),
		MessagesPath:             messagesPath,
		MessagesBackupPath:       messagesBackupPath,
		UsageCachePath:           usageCachePath,
		StagingDir:               stagingDir,
		StoragePaths:             storagePaths,
		WorkingDir:               cwd,
		SessionFiles:             sessionFiles,
		FilePolicy:               filePolicy,
		NativeExec:               nativeExec,
		NativeReadImage:          nativeReadImage,
		NativeLSP:                nativeLSP,
		ExtensionWebSearch:       extensionWebSearch,
		NativeAsync:              nativeAsync,
		NativeTask:               nativeTask,
		NativeCodeContext:        nativeCodeContext,
		NativeRecords:            nativeRecords,
		NativeRequestDraft:       nativeRequestDraft,
		NativeSkill:              nativeSkill,
		ExtensionDecideSystemone: extensionDecideSystemone,
		NativeSubagent:           nativeSubagent,
		NativeProfileMessage:     nativeProfileMessage,
		NativeToolSmith:          nativeToolSmith,
		NativeTranscript:         nativeTranscript,
		NativeTranscripts:        nativeTranscripts,
		NativeProfile:            nativeProfile,
		NativeLoadTool:           nativeLoadTool,
		GlobalJadRoot:            location.Root.GlobalJadRoot,
		MCPServers:               mcpServers,
		StateContext:             stateContext,
		HTTPDebug:                httpDebug,
		ToolDefinitionPaths:      toolDefinitionPaths,
		MaxToolRounds:            maxToolRounds,
		MaxParallelToolCalls:     maxParallelToolCalls,
		ToolCallScheduling:       toolCallScheduling,
		ProviderRetry:            providerRetry,
		ProviderStream:           providerStream,
		ProviderNetwork:          providerNetwork,
		MaxRunTokens:             maxRunTokens,
		ModelFallback:            modelFallback,
		Sandbox:                  sandbox,
		Isolation:                isolation,
		Budget:                   budget,
		ProjectInstructions:      projectInstructions,
		MaxRunCostUSD:            maxRunCost,
		MaxRunDurationMS:         maxRunDuration,
		Checkpoint:               checkpoint,
		PlanMode:                 planMode,
		Hooks:                    hooks,
		ProfileTools:             profileTools,
		ImageResize:              imageResize,
		ImageStore:               imageStore,
		BackupTurns:              backupTurns,
		Compaction:               compactionSettings,
		Persistence:              persistence,
		FrontCompaction:          frontCompaction,
		FrontCompactionConfig:    frontCompactionConfig,
		// The model file is read by WithModel, so a settings-only caller is not
		// affected by a missing or malformed model.json. Only the prompt, which
		// comes from the settings, is filled in here.
		Model:                   Model{PromptRole: promptRole, PromptText: promptText},
		PromptDocuments:         promptDocuments,
		ProjectInstructionFiles: instructionFiles,
	}
	if location.Root.Layout != nil {
		config.hideFixedPaths(location.Root.Layout.FixedHidePaths, nil)
		config.Libraries = location.Root.Layout.Libraries
	}
	return config, nil
}

// LoadSessionConfig loads the session settings and the selected model file. Use
// LoadSessionSettings when the model is not needed yet.
//
// A session whose own default model cannot be used (ModelFault, with no model requested) still
// loads, with no model read, so that its history and files stay readable; a caller that
// runs the model gets the error from WithModel. A model the caller requests must load.
func LoadSessionConfig(location Location, requestedProfile, requestedModel string) (SessionConfig, error) {
	config, err := LoadSessionSettings(location, requestedProfile, requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	if config.ModelFault != nil && requestedModel == "" {
		return config, nil
	}
	return config.WithModel()
}

// WithModel returns the configuration with the model file read and validated.
// The file is the one LoadSessionSettings selected (ModelPath).
func (c SessionConfig) WithModel() (SessionConfig, error) {
	if c.RemoteKeyFault != nil {
		return SessionConfig{}, c.RemoteKeyFault
	}
	if c.ModelPath == "" && c.ModelFault != nil {
		return SessionConfig{}, c.ModelFault
	}
	model, err := loadModelOf(c.ModelPath, c.ModelBases, c.WorkspaceLayout, c.Model.PromptRole, c.Model.PromptText)
	if err != nil {
		return SessionConfig{}, err
	}
	if model, err = model.withEffort(c.ReasoningEffort); err != nil {
		return SessionConfig{}, markOwnEffort(c.ModelPath, c.ModelBases, err)
	}
	c.Model = model
	return c, nil
}

func selectProfile(setting map[string]json.RawMessage, requested, path string) (string, map[string]json.RawMessage, error) {
	paths, err := objectField(setting, "paths", path)
	if err != nil {
		return "", nil, err
	}
	var profiles map[string]json.RawMessage
	profilesRaw := paths["profiles"]
	if err := json.Unmarshal(profilesRaw, &profiles); err != nil || len(profiles) == 0 {
		return "", nil, configError("%s: paths.profiles must be a non-empty object", path)
	}
	active, err := requiredStringField(paths, "active_profile", path+": paths")
	if err != nil {
		return "", nil, err
	}
	if !profileNamePattern.MatchString(active) {
		return "", nil, configError("%s: paths.active_profile must contain only letters, numbers, underscores, or hyphens", path)
	}
	selected := requested
	if selected == "" {
		selected = active
	}
	if !profileNamePattern.MatchString(selected) {
		return "", nil, configError("%s: path profile '%s' must contain only letters, numbers, underscores, or hyphens", path, selected)
	}
	raw, exists := profiles[selected]
	if !exists {
		return "", nil, configError("%s: path profile '%s' is not defined in paths.profiles", path, selected)
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(raw, &profile); err != nil || profile == nil {
		return "", nil, configError("%s: paths.profiles.%s must be an object", path, selected)
	}
	names, err := orderedObjectKeys(profilesRaw)
	if err != nil {
		return "", nil, configError("%s: paths.profiles must be a non-empty object", path)
	}
	for _, name := range names {
		if !profileNamePattern.MatchString(name) {
			return "", nil, configError("%s: invalid paths.profiles key '%s'", path, name)
		}
		if err := validateProfileFields(name, profiles[name], path); err != nil {
			return "", nil, err
		}
	}
	return selected, profile, nil
}

func normalizePersistence(raw json.RawMessage, path string) (Persistence, error) {
	settings := Persistence{
		Durability:                 "turn",
		MaxItemBytes:               defaultMaxItemBytes,
		ResponseMetadata:           "minimal",
		CursorCacheCheckpointBytes: defaultCursorCacheCheckpointBytes,
		RunErrorCacheMaxEntries:    defaultRunErrorCacheMaxEntries,
	}
	if len(raw) == 0 {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Persistence{}, configError("%s: persistence must be an object", path)
	}
	if value, ok := fields["durability"]; ok {
		if err := json.Unmarshal(value, &settings.Durability); err != nil {
			return Persistence{}, configError("%s: persistence.durability must be process, turn, or strict", path)
		}
	}
	if settings.Durability != "process" && settings.Durability != "turn" && settings.Durability != "strict" {
		return Persistence{}, configError("%s: persistence.durability must be process, turn, or strict", path)
	}
	if value, ok := fields["max_item_bytes"]; ok && string(value) != "null" {
		configured, valid := configSafeInteger(value)
		if !valid || configured < 1 || configured > int64(maxConfiguredItemBytes) {
			return Persistence{}, configError("%s: persistence.max_item_bytes must be a safe integer between 1 and %d", path, maxConfiguredItemBytes)
		}
		settings.MaxItemBytes = configured
	}
	if value, ok := fields["response_metadata"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &settings.ResponseMetadata); err != nil {
			return Persistence{}, configError("%s: persistence.response_metadata must be minimal or full", path)
		}
	}
	if settings.ResponseMetadata != "minimal" && settings.ResponseMetadata != "full" {
		return Persistence{}, configError("%s: persistence.response_metadata must be minimal or full", path)
	}
	if value, ok := fields["cursor_cache_checkpoint_bytes"]; ok && string(value) != "null" {
		var checkpointBytes float64
		if err := json.Unmarshal(value, &checkpointBytes); err != nil || math.IsNaN(checkpointBytes) || math.IsInf(checkpointBytes, 0) || math.Trunc(checkpointBytes) != checkpointBytes || checkpointBytes < 1 || checkpointBytes > float64(maxConfiguredCursorCacheCheckpointBytes) {
			return Persistence{}, configError("%s: persistence.cursor_cache_checkpoint_bytes must be a safe integer between 1 and %d", path, maxConfiguredCursorCacheCheckpointBytes)
		}
		settings.CursorCacheCheckpointBytes = int64(checkpointBytes)
	}
	if value, ok := fields["run_error_cache_max_entries"]; ok && string(value) != "null" {
		configured, valid := configSafeInteger(value)
		if !valid || configured < 1 || configured > maxConfiguredRunErrorCacheMaxEntries {
			return Persistence{}, configError("%s: persistence.run_error_cache_max_entries must be a safe integer between 1 and %d", path, maxConfiguredRunErrorCacheMaxEntries)
		}
		settings.RunErrorCacheMaxEntries = int(configured)
	}
	return settings, nil
}

func normalizeCompaction(raw json.RawMessage, path string) (Compaction, error) {
	settings := Compaction{
		AutoEnabled:     defaultAutoCompactionEnabled,
		TriggerTokens:   defaultCompactionTriggerTokens,
		TurnType:        "assistant_final",
		KeepTurns:       1,
		BackupMode:      "append",
		KeepBackupTurns: defaultKeepBackupTurns,
		Overflow:        DefaultCompactionOverflow(),
	}
	if len(raw) == 0 {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Compaction{}, configError("%s: compaction must be an object", path)
	}
	if value, ok := fields["auto_enabled"]; ok && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.AutoEnabled); err != nil {
			return Compaction{}, configError("%s: compaction.auto_enabled must be a boolean", path)
		}
	}
	var err error
	settings.TriggerTokens, err = configuredInteger(fields, "trigger_tokens", defaultCompactionTriggerTokens, 1, maxCompactionTriggerTokens, path, "compaction")
	if err != nil {
		return Compaction{}, err
	}
	if value, ok := fields["backup_mode"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &settings.BackupMode); err != nil || (settings.BackupMode != "replace" && settings.BackupMode != "append") {
			return Compaction{}, configError("%s: compaction.backup_mode must be replace or append", path)
		}
	}
	if value, ok := fields["keepbatch"]; ok && string(value) != "null" {
		var batch map[string]json.RawMessage
		if err := json.Unmarshal(value, &batch); err != nil || batch == nil {
			return Compaction{}, configError("%s: compaction.keepbatch must be an object", path)
		}
		if tools, ok := batch["tools"]; ok && string(tools) != "null" {
			configured, valid := configSafeInteger(tools)
			if !valid || configured < 0 || configured > 1000 {
				return Compaction{}, configError("%s: compaction.keepbatch.tools must be an integer between 0 and 1000", path)
			}
			settings.KeepToolPairs = int(configured)
		}
		if turnValue, ok := batch["turn"]; ok && string(turnValue) != "null" {
			var turn map[string]json.RawMessage
			if err := json.Unmarshal(turnValue, &turn); err != nil || turn == nil {
				return Compaction{}, configError("%s: compaction.keepbatch.turn must be an object", path)
			}
			if value, ok := turn["type"]; ok && string(value) != "null" {
				if err := json.Unmarshal(value, &settings.TurnType); err != nil || (settings.TurnType != "assistant_final" && settings.TurnType != "full") {
					return Compaction{}, configError("%s: compaction.keepbatch.turn.type must be assistant_final or full", path)
				}
			}
			if value, ok := turn["keep"]; ok && string(value) != "null" {
				configured, valid := configSafeInteger(value)
				if !valid || configured < 1 || configured > 1000 {
					return Compaction{}, configError("%s: compaction.keepbatch.turn.keep must be an integer between 1 and 1000", path)
				}
				settings.KeepTurns = int(configured)
			}
		}
	}
	if value, ok := fields["keepbackup"]; ok && string(value) != "null" {
		var keepbackup map[string]json.RawMessage
		if err := json.Unmarshal(value, &keepbackup); err != nil || keepbackup == nil {
			return Compaction{}, configError("%s: compaction.keepbackup must be an object", path)
		}
		if value, ok := keepbackup["turns"]; ok && string(value) != "null" {
			configured, valid := configSafeInteger(value)
			if !valid || configured < 0 || configured > int64(maxKeepBackupTurns) {
				return Compaction{}, configError("%s: compaction.keepbackup.turns must be an integer between 0 and %d", path, maxKeepBackupTurns)
			}
			settings.KeepBackupTurns = int(configured)
		}
		if value, ok := keepbackup["append_limit"]; ok && string(value) != "null" {
			configured, valid := configSafeInteger(value)
			if !valid || configured < 0 || configured > int64(maxKeepBackupTurns) {
				return Compaction{}, configError("%s: compaction.keepbackup.append_limit must be an integer between 0 and %d", path, maxKeepBackupTurns)
			}
			settings.AppendLimit = int(configured)
		}
	}
	if settings.Overflow, err = normalizeCompactionOverflow(fields["overflow"], path); err != nil {
		return Compaction{}, err
	}
	return settings, nil
}

func normalizeFrontCompaction(raw json.RawMessage, exists bool, path string) (json.RawMessage, FrontCompactionSettings, error) {
	settings := FrontCompactionSettings{Enabled: true, DeleteMode: "permanent"}
	fields := make(map[string]json.RawMessage)
	if exists {
		if len(raw) == 0 || string(raw) == "null" {
			return nil, FrontCompactionSettings{}, configError("%s: front_compaction must be an object", path)
		}
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, FrontCompactionSettings{}, configError("%s: front_compaction must be an object", path)
		}
	}
	if value, ok := fields["enabled"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &settings.Enabled); err != nil {
			return nil, FrontCompactionSettings{}, configError("%s: front_compaction.enabled must be a boolean", path)
		}
	}
	if value, ok := fields["delete_mode"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &settings.DeleteMode); err != nil || (settings.DeleteMode != "permanent" && settings.DeleteMode != "recoverable") {
			return nil, FrontCompactionSettings{}, configError("%s: front_compaction.delete_mode must be permanent or recoverable", path)
		}
	}
	encodedEnabled, err := json.Marshal(settings.Enabled)
	if err != nil {
		return nil, FrontCompactionSettings{}, err
	}
	encodedDeleteMode, err := json.Marshal(settings.DeleteMode)
	if err != nil {
		return nil, FrontCompactionSettings{}, err
	}
	fields["enabled"] = encodedEnabled
	fields["delete_mode"] = encodedDeleteMode
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, FrontCompactionSettings{}, err
	}
	return encoded, settings, nil
}

func loadModel(path, promptRole, promptText string) (Model, error) {
	fields, err := readSessionObject(path)
	if err != nil {
		return Model{}, err
	}
	return modelFromFields(fields, path, promptRole, promptText)
}

// modelFromFields applies the model file rules to an already decoded file; path only names
// the file in errors and anchors relative paths.
func modelFromFields(fields map[string]json.RawMessage, path, promptRole, promptText string) (Model, error) {
	var err error
	if err := validateSchemaVersion(fields, path); err != nil {
		return Model{}, err
	}
	if _, own := fields["based_on"]; own {
		// Only the session of the profile an own model belongs to composes it with its base
		// (loadModelOf); read as a whole model anywhere else, it fails here.
		return Model{}, configError("%s: based_on is a key of a profile's own model (profile-own/<profile>/models), which only that profile's session loads; a model file used on its own cannot set it", path)
	}
	if raw, ok := fields["name"]; ok {
		var displayName string
		if json.Unmarshal(raw, &displayName) != nil {
			return Model{}, configError("%s: name must be a string", path)
		}
	}
	provider, err := stringField(fields, "provider", "openai", path)
	if err != nil {
		return Model{}, err
	}
	modelName, err := requiredStringField(fields, "model", path)
	if err != nil {
		return Model{}, err
	}
	baseURL := ""
	if raw, ok := fields["base_url"]; ok {
		if err := json.Unmarshal(raw, &baseURL); err != nil || strings.TrimSpace(baseURL) == "" {
			return Model{}, configError("%s: base_url must be a non-empty string", path)
		}
		baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Model{}, configError("%s: base_url must use http or https", path)
		}
	}
	if err := rejectRemovedOAuthFields(fields, path); err != nil {
		return Model{}, err
	}
	api, anthropicConfig, chatConfig, err := modelAPIFields(fields, path, baseURL)
	if err != nil {
		return Model{}, err
	}
	apiKeyEnvDefault := "OPENAI_API_KEY"
	if strings.EqualFold(provider, "openrouter") || strings.Contains(baseURL, "openrouter.ai") {
		apiKeyEnvDefault = "OPENROUTER_API_KEY"
	}
	if api == APIAnthropicMessages {
		apiKeyEnvDefault = "ANTHROPIC_API_KEY"
	}
	apiKeyEnv, err := stringField(fields, "api_key_env", apiKeyEnvDefault, path)
	if err != nil {
		return Model{}, err
	}
	if !environmentNamePattern.MatchString(apiKeyEnv) {
		return Model{}, configError("%s: api_key_env is not a valid environment variable name", path)
	}
	if _, exists := fields["instructions"]; exists {
		return Model{}, configError("%s: instructions was removed from the model file; put the text in the profile's instructions (or a prompt document), so it is written once for every model the profile offers", path)
	}
	request := json.RawMessage(`{}`)
	if raw, ok := fields["request"]; ok && string(raw) != "null" {
		var requestFields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &requestFields); err != nil || requestFields == nil {
			return Model{}, configError("%s: request must be a JSON object", path)
		}
		for _, reserved := range []string{"model", "input", "instructions", "stream", "tools"} {
			if _, exists := requestFields[reserved]; exists {
				return Model{}, configError("%s: request.%s is managed by the session runner", path, reserved)
			}
		}
		request = bytes.TrimSpace(raw)
	}
	hostedTools, err := hostedToolsField(fields, path)
	if err != nil {
		return Model{}, err
	}
	if len(hostedTools) > 0 && api != APIResponses {
		return Model{}, configError("%s: hosted_tools needs api responses: the %s translation serves function tools only", path, api)
	}
	efforts, err := effortsField(fields, path)
	if err != nil {
		return Model{}, err
	}
	pricing, err := modelPricingField(fields, path)
	if err != nil {
		return Model{}, err
	}
	promptCacheKey, err := promptCacheKeyField(fields, path, api, request)
	if err != nil {
		return Model{}, err
	}
	return Model{
		PromptCacheKey:  promptCacheKey,
		Pricing:         pricing,
		Efforts:         efforts,
		Provider:        provider,
		Name:            modelName,
		APIKeyEnv:       apiKeyEnv,
		BaseURL:         baseURL,
		Request:         request,
		HostedTools:     hostedTools,
		PromptRole:      promptRole,
		PromptText:      promptText,
		API:             api,
		Anthropic:       anthropicConfig,
		ChatCompletions: chatConfig,
	}, nil
}

// promptCacheKeyField reads prompt_cache_key: "off" (the default) or "session". It is an
// OpenAI Responses field, so it needs api responses, and it cannot be set twice: a
// request.prompt_cache_key already sends a fixed key.
func promptCacheKeyField(fields map[string]json.RawMessage, path, api string, request json.RawMessage) (string, error) {
	mode, err := stringField(fields, "prompt_cache_key", PromptCacheKeyOff, path)
	if err != nil {
		return "", err
	}
	if mode != PromptCacheKeyOff && mode != PromptCacheKeySession {
		return "", configError("%s: prompt_cache_key must be %q or %q", path, PromptCacheKeyOff, PromptCacheKeySession)
	}
	if mode == PromptCacheKeyOff {
		return mode, nil
	}
	if api != APIResponses {
		return "", configError("%s: prompt_cache_key %q needs api responses", path, mode)
	}
	var requestFields map[string]json.RawMessage
	if json.Unmarshal(request, &requestFields) == nil {
		if _, exists := requestFields["prompt_cache_key"]; exists {
			return "", configError("%s: prompt_cache_key %q and request.prompt_cache_key both set the key; keep one", path, mode)
		}
	}
	return mode, nil
}

// hostedToolsField reads hosted_tools: an optional array of tool objects the provider
// runs itself. Each needs a non-empty string type; "function" is refused because
// function tools come from the tool catalog. The objects are otherwise passed on
// unchanged, so a provider-specific option needs no code here.
func hostedToolsField(fields map[string]json.RawMessage, path string) ([]json.RawMessage, error) {
	raw, ok := fields["hosted_tools"]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, configError("%s: hosted_tools must be an array of tool objects", path)
	}
	tools := make([]json.RawMessage, 0, len(entries))
	for index, entry := range entries {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(entry, &object); err != nil || object == nil {
			return nil, configError("%s: hosted_tools[%d] must be an object", path, index)
		}
		var toolType string
		if err := json.Unmarshal(object["type"], &toolType); err != nil || strings.TrimSpace(toolType) == "" {
			return nil, configError("%s: hosted_tools[%d].type must be a non-empty string", path, index)
		}
		if strings.EqualFold(strings.TrimSpace(toolType), "function") {
			return nil, configError("%s: hosted_tools[%d].type must not be function; function tools come from the tool catalog", path, index)
		}
		tools = append(tools, entry)
	}
	if len(tools) == 0 {
		return nil, nil
	}
	return tools, nil
}

// loadPrompt reads the prompt. workspace is W in the workspace layout, where a prompt
// document may start with {workspace} (design D1, D2); empty in v1, where the text is a path
// as written.
func loadPrompt(setting, profile map[string]json.RawMessage, cwd, workspace, path string, overlay *documentOverlay) (string, string, []string, error) {
	role := "system"
	if raw, ok := setting["prompt_role"]; ok {
		if err := json.Unmarshal(raw, &role); err != nil || (role != "system" && role != "developer") {
			return "", "", nil, configError("%s: prompt_role must be system or developer", path)
		}
	}
	var documents []string
	if raw, ok := setting["prompt_documents"]; ok {
		if err := json.Unmarshal(raw, &documents); err != nil || documents == nil {
			return "", "", nil, configError("%s: prompt_documents must be an array", path)
		}
	}
	if raw, ok := profile["prompt_documents"]; ok {
		var profileDocuments []string
		if err := json.Unmarshal(raw, &profileDocuments); err != nil || profileDocuments == nil {
			return "", "", nil, configError("%s: paths.profiles prompt_documents must be an array", path)
		}
		documents = append(documents, profileDocuments...)
	}
	texts := make([]string, 0, len(documents)+1)
	if raw, ok := profile["instructions"]; ok {
		text, valid := nonEmptyJSONString(raw)
		if !valid {
			return "", "", nil, configError("%s: paths.profiles instructions must be a non-empty string", path)
		}
		texts = append(texts, text)
	}
	var paths []string
	for index, document := range documents {
		if workspace != "" {
			expanded, _, err := cutPlaceholder(document, workspacePlaceholder, workspace)
			if err != nil {
				return "", "", nil, configError("%s: prompt_documents[%d]: %v", path, index, err)
			}
			document = expanded
		}
		contents, resolved, err := readPromptDocument(document, index, "prompt_documents", cwd, path, maxPromptDocumentBytes, overlay)
		if err != nil {
			return "", "", nil, err
		}
		texts = append(texts, contents)
		paths = append(paths, resolved)
	}
	return role, strings.Join(texts, "\n\n"), paths, nil
}

// readPromptDocument reads one prompt document: ~ is expanded and a relative path
// resolves from cwd. field names the setting in errors. A trial load's overlay (nil
// otherwise) answers for the documents a profile edit would write or delete.
func readPromptDocument(document string, index int, field, cwd, path string, limit int64, overlay *documentOverlay) (contents, resolvedPath string, err error) {
	if strings.TrimSpace(document) == "" {
		return "", "", configError("%s: %s[%d] must be a non-empty string", path, field, index)
	}
	resolved := expandHome(document)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cwd, resolved)
	}
	resolved = filepath.Clean(resolved)
	data, err := overlay.read(resolved, min(limit, maxPromptDocumentBytes))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", configError("%s: %s[%d] could not be read: %s", path, field, index, resolved)
		}
		return "", "", configError("%s: %s[%d] could not be read: %v", path, field, index, err)
	}
	return string(data), resolved, nil
}

func sessionCWD(setting map[string]json.RawMessage, fallback, path string) (string, error) {
	cwd := fallback
	if raw, ok := setting["cwd"]; ok {
		if err := json.Unmarshal(raw, &cwd); err != nil || strings.TrimSpace(cwd) == "" {
			return "", configError("%s: cwd must be a non-empty string", path)
		}
	}
	cwd = expandHome(strings.TrimSpace(cwd))
	if !filepath.IsAbs(cwd) {
		return "", configError("%s: cwd must be an absolute path", path)
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", configError("%s: cwd does not exist or is not a directory: %s", path, cwd)
	}
	return cwd, nil
}

func resolveConfiguredPath(fields map[string]json.RawMessage, name, fallback, base string) (string, error) {
	value := fallback
	if raw, ok := fields[name]; ok {
		if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
			return "", configError("paths.%s must be a non-empty string", name)
		}
	}
	return resolveFrom(base, expandHome(strings.TrimSpace(value))), nil
}

// readSessionObject mirrors the TypeScript readObject: a missing or blank file is
// an error. The text of an invalid-JSON error carries Go's decoder message, which cannot
// match V8's.
func readSessionObject(path string) (map[string]json.RawMessage, error) {
	contents, err := readRegularFile(path, maxInternalJSONFileBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, configError("%s: file is required", path)
		}
		return nil, configError("%s: could not read configuration: %v", path, err)
	}
	return parseSessionObject(path, contents)
}

func parseSessionObject(path string, contents []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, configError("%s: file is empty", path)
	}
	var value any
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, configError("%s: invalid JSON: %v", path, err)
	}
	if _, isObject := value.(map[string]any); !isObject {
		return nil, configError("%s: expected a JSON object", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		return nil, configError("%s: invalid JSON: %v", path, err)
	}
	return fields, nil
}

func validateSchemaVersion(fields map[string]json.RawMessage, path string) error {
	raw, ok := fields["schema_version"]
	if !ok {
		return nil
	}
	version, valid := configSafeInteger(raw)
	if !valid || version != 1 {
		return configError("%s: unsupported schema_version %s", path, strings.TrimSpace(string(raw)))
	}
	return nil
}

func objectField(fields map[string]json.RawMessage, name, path string) (map[string]json.RawMessage, error) {
	raw, ok := fields[name]
	if !ok {
		return map[string]json.RawMessage{}, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, configError("%s: %s must be a JSON object", path, name)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, configError("%s: %s must be a JSON object", path, name)
	}
	return value, nil
}

func orderedObjectKeys(raw json.RawMessage) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	keys := make([]string, 0)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return keys, nil
}

func requiredStringField(fields map[string]json.RawMessage, name, path string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", configError("%s: %s must be a non-empty string", path, name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: %s must be a non-empty string", path, name)
	}
	return strings.TrimSpace(value), nil
}

func stringField(fields map[string]json.RawMessage, name, fallback, path string) (string, error) {
	raw, ok := fields[name]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: %s must be a non-empty string", path, name)
	}
	return strings.TrimSpace(value), nil
}

func stringFieldOr(fields map[string]json.RawMessage, name, fallback string) string {
	var value string
	if raw, ok := fields[name]; ok && json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return fallback
}

func expandHome(value string) string {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	return value
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func configError(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}
