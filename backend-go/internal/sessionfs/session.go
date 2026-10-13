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
	// AuthMode is "api_key" (the key is read from APIKeyEnv) or "openai_oauth" (the
	// credential is read from OAuthAuthFile).
	AuthMode string
	// OAuthAuthFile is the absolute credential file path; set only for openai_oauth.
	OAuthAuthFile string
	Request       json.RawMessage
	// HostedTools are provider-side tool definitions (for example {"type":"web_search"})
	// the request carries after the function tools. They have no tool code here: the
	// provider runs them. Empty means none.
	HostedTools []json.RawMessage
	PromptRole  string
	PromptText  string
}

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
	SessionName string
	// Category groups sessions in a listing (session.json "category"); empty when unset.
	// It does not change the session id or any path.
	Category       string
	Profile        string
	DefaultProfile string
	Profiles       []string
	ModelPath      string
	ModelDefault   string
	Models         []string
	SettingPath    string
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
	NativeReadPolicy   NativePathPolicy
	NativeRead         *NativeReadSettings
	NativeGrep         *NativeGrepSettings
	NativeWrite        *NativeWriteSettings
	NativeEdit         *NativeEditSettings
	NativeBash         *NativeBashSettings
	NativeExec         *NativeExecSettings
	NativeGlob         *NativeGlobSettings
	NativeReadImage    *NativeReadImageSettings
	NativeProcess      *NativeProcessSettings
	NativeTask         *NativeTaskSettings
	NativeMemory       *NativeMemorySettings
	NativeFetch        *NativeFetchSettings
	NativeRequestDraft *NativeRequestDraftSettings
	NativeSkill        *NativeSkillSettings
	NativeDecide       *NativeDecideSettings
	NativeSubagent     *NativeSubagentSettings
	// AgentToolsets holds, by "<category>/<name>", the settings of each toolset a
	// sub-agent names in its toolset key. Only the tool settings and MCP servers of an
	// entry are used; it has no subagent tool.
	AgentToolsets map[string]SessionConfig
	// GlobalJadRoot is the global root whose setting.json names the skill
	// directories the skill tool reads.
	GlobalJadRoot       string
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
	// OpenAIOAuth configures requests of a model file whose auth_mode is openai_oauth.
	OpenAIOAuth OpenAIOAuthSettings
	// MaxRunTokens is the most tokens one run may use before it starts another
	// provider round; zero means no cap (the key is absent).
	MaxRunTokens int64
	// Approval holds the wait and preview values for tools whose approval is "ask".
	Approval ApprovalSettings
	// Checkpoint controls run checkpoints and undo; it is off by default.
	Checkpoint CheckpointSettings
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
}

// LoadSessionSettings reads and validates a catalog-layout session (session.json plus the
// profile, model and toolset files it names) and selects, but does not read, the model file
// (SessionConfig.ModelPath).
// RequestedModel is the model config path from the run body. Callers that need
// the provider settings call WithModel afterwards; the TypeScript backend also
// defers the model file until a run needs it, which is why a session with a
// broken model.json still lists, snapshots and accepts a user message.
func LoadSessionSettings(location Location, requestedProfile, requestedModel string) (SessionConfig, error) {
	if location.UsesLayout() {
		return LoadLayoutSessionSettings(location, *location.Root.Layout, requestedProfile, requestedModel)
	}
	return SessionConfig{}, configError("%s: not a catalog-layout session; it needs a <session id>.json file in a session root and a root setting.json that names the catalogs", location.File)
}

// loadSessionSettingsFrom validates a setting-shaped map and turns it into a
// SessionConfig. settingPath names the file the map came from in error messages and
// is the base for relative storage paths. The layout loader (layout_load.go) feeds
// it a map composed from the catalogs, so both layouts share every normalizer and
// applyApprovalDefault. envFileBase is the folder a relative tools.native.*.env_file is
// resolved against: the session folder for a legacy session, the global root for a
// catalog-layout session, whose toolset many sessions share.
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

	profile, profileSettings, profileConfigured, err := selectProfile(setting, requestedProfile, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	paths, err := objectField(setting, "paths", settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	defaultProfile := stringFieldOr(paths, "active_profile", "")
	var pathProfiles []string
	if profiles, exists := paths["profiles"]; exists {
		pathProfiles, err = orderedObjectKeys(profiles)
		if err != nil {
			return SessionConfig{}, configError("%s: paths.profiles must be a JSON object", settingPath)
		}
	}
	basePaths := paths
	if profileConfigured {
		basePaths = make(map[string]json.RawMessage)
		for name, value := range paths {
			if name != "profiles" && name != "active_profile" {
				basePaths[name] = value
			}
		}
	}
	if err := validateBasePathFields(basePaths, settingPath); err != nil {
		return SessionConfig{}, err
	}
	selectedPaths, profileStorageDir := selectProfileStoragePaths(location.StateDir(), basePaths, profileSettings, profile, defaultProfile, profileConfigured)
	settingDir := filepath.Dir(settingPath)
	if err := validateAllStoragePaths(location, settingPath, paths, basePaths, pathProfiles, defaultProfile, profileConfigured); err != nil {
		return SessionConfig{}, err
	}
	storagePaths, err := resolveStoragePaths(selectedPaths, profileStorageDir, settingDir, profileConfigured)
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
	toolStorageDir, err := normalizeToolStorageDir(profileSettings, profile, profileConfigured, cwd, settingPath)
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
	openAIOAuth, err := normalizeOpenAIOAuthSettings(setting["openai_oauth"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	maxRunTokens, err := normalizeMaxRunTokens(setting["max_run_tokens"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	approval, err := normalizeApprovalSettings(setting["approval"], settingPath)
	if err != nil {
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
	var profileTools ProfileToolSettings
	if raw, ok := profileSettings["tools"]; ok && profileConfigured {
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
	toolDefinitionPaths, err := normalizeToolDefinitionPaths(setting["tool_definition_paths"], location.Root.GlobalJadRoot, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	mcpServers, err := normalizeMCPServers(setting["mcp_servers"], cwd, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeRead, err := normalizeNativeReadSettings(setting, location.Root.GlobalJadRoot, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeReadPolicy := NativePathPolicy{Paths: []NativePathRule{}}
	if nativeRead != nil {
		nativeReadPolicy = nativeRead.Policy
	}
	nativeGrep, err := normalizeNativeGrepSettings(
		setting,
		location.Root.GlobalJadRoot,
		settingPath,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeGlob, err := normalizeNativeGlobSettings(
		setting,
		location.Root.GlobalJadRoot,
		settingPath,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeReadImage, err := normalizeNativeReadImageSettings(setting, location.Root.GlobalJadRoot, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeWrite, nativeEdit, err := normalizeNativeWriteEditSettings(
		setting,
		location.Root.GlobalJadRoot,
		settingPath,
		cwd,
		location.StateDir(),
		storagePaths.ToolStorage,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	grepPolicy := NativePathPolicy{Paths: []NativePathRule{}}
	if nativeGrep != nil {
		grepPolicy = nativeGrep.Policy
	}
	nativeBash, err := normalizeNativeBashSettings(
		setting,
		location.Root.GlobalJadRoot,
		settingPath,
		cwd,
		location.StateDir(),
		storagePaths.ToolStorage,
		envFileBase,
		nativeReadPolicy,
		grepPolicy,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeExec, err := normalizeNativeExecSettings(setting, location.Root.GlobalJadRoot, settingPath, cwd, location.StateDir(), storagePaths.ToolStorage, envFileBase, nativeReadPolicy, grepPolicy)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeTask, err := normalizeNativeTaskSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeMemory, err := normalizeNativeMemorySettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeFetch, err := normalizeNativeFetchSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
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
	nativeDecide, err := normalizeNativeDecideSettings(setting, location.Root.GlobalJadRoot, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeSubagent, err := normalizeNativeSubagentSettings(setting, settingPath, cwd, storagePaths.ToolStorage)
	if err != nil {
		return SessionConfig{}, err
	}
	nativeProcess, err := normalizeNativeProcessSettings(
		setting,
		location.Root.GlobalJadRoot,
		settingPath,
		cwd,
		location.StateDir(),
		storagePaths.ToolStorage,
		envFileBase,
	)
	if err != nil {
		return SessionConfig{}, err
	}
	applyApprovalDefault(approval.Default, setting, nativeWrite, nativeEdit, nativeBash, nativeExec, nativeProcess, mcpServers)
	stateContext, err := normalizeStateContextSettings(setting, settingPath, cwd, nativeProcess, nativeTask, nativeMemory, nativeSubagent)
	if err != nil {
		return SessionConfig{}, err
	}
	httpDebug, err := normalizeHTTPDebugSettings(setting["http_debug"], settingPath)
	if err != nil {
		return SessionConfig{}, err
	}
	promptRole, promptText, err := loadPrompt(setting, profileSettings, profileConfigured, cwd, settingPath)
	if err != nil {
		return SessionConfig{}, err
	}

	modelPathValue := "model.json"
	modelDefault := ""
	allowedModels, hasAllowedModels := []string(nil), false
	hasModelDefault := false
	if profileConfigured {
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
			modelPathValue = strings.TrimSpace(configured)
			modelDefault = modelPathValue
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
	}
	if requestedModel != "" {
		if !hasAllowedModels || !containsString(allowedModels, requestedModel) {
			return SessionConfig{}, &ClientError{Message: "model config path is not available in the active session path profile"}
		}
		modelPathValue = requestedModel
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
	return SessionConfig{
		SessionName:           sessionName,
		Profile:               profile,
		DefaultProfile:        defaultProfile,
		Profiles:              pathProfiles,
		ModelPath:             resolvedModelPath,
		ModelDefault:          modelDefault,
		Models:                models,
		SettingPath:           settingPath,
		StateDir:              location.StateDir(),
		MessagesPath:          messagesPath,
		MessagesBackupPath:    messagesBackupPath,
		UsageCachePath:        usageCachePath,
		StagingDir:            stagingDir,
		StoragePaths:          storagePaths,
		WorkingDir:            cwd,
		SessionFiles:          sessionFiles,
		NativeReadPolicy:      nativeReadPolicy,
		NativeRead:            nativeRead,
		NativeGrep:            nativeGrep,
		NativeWrite:           nativeWrite,
		NativeEdit:            nativeEdit,
		NativeBash:            nativeBash,
		NativeExec:            nativeExec,
		NativeGlob:            nativeGlob,
		NativeReadImage:       nativeReadImage,
		NativeProcess:         nativeProcess,
		NativeTask:            nativeTask,
		NativeMemory:          nativeMemory,
		NativeFetch:           nativeFetch,
		NativeRequestDraft:    nativeRequestDraft,
		NativeSkill:           nativeSkill,
		NativeDecide:          nativeDecide,
		NativeSubagent:        nativeSubagent,
		GlobalJadRoot:         location.Root.GlobalJadRoot,
		MCPServers:            mcpServers,
		StateContext:          stateContext,
		HTTPDebug:             httpDebug,
		ToolDefinitionPaths:   toolDefinitionPaths,
		MaxToolRounds:         maxToolRounds,
		MaxParallelToolCalls:  maxParallelToolCalls,
		ToolCallScheduling:    toolCallScheduling,
		ProviderRetry:         providerRetry,
		OpenAIOAuth:           openAIOAuth,
		MaxRunTokens:          maxRunTokens,
		Approval:              approval,
		Checkpoint:            checkpoint,
		Hooks:                 hooks,
		ProfileTools:          profileTools,
		ImageResize:           imageResize,
		ImageStore:            imageStore,
		BackupTurns:           backupTurns,
		Compaction:            compactionSettings,
		Persistence:           persistence,
		FrontCompaction:       frontCompaction,
		FrontCompactionConfig: frontCompactionConfig,
		// The model file is read by WithModel, so a settings-only caller is not
		// affected by a missing or malformed model.json. Only the prompt, which
		// comes from the settings, is filled in here.
		Model: Model{PromptRole: promptRole, PromptText: promptText},
	}, nil
}

// LoadSessionConfig loads the session settings and the selected model file. Use
// LoadSessionSettings when the model is not needed yet.
func LoadSessionConfig(location Location, requestedProfile, requestedModel string) (SessionConfig, error) {
	config, err := LoadSessionSettings(location, requestedProfile, requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	return config.WithModel()
}

// WithModel returns the configuration with the model file read and validated.
// The file is the one LoadSessionSettings selected (ModelPath).
func (c SessionConfig) WithModel() (SessionConfig, error) {
	model, err := loadModel(c.ModelPath, c.Model.PromptRole, c.Model.PromptText)
	if err != nil {
		return SessionConfig{}, err
	}
	c.Model = model
	return c, nil
}

func selectProfile(setting map[string]json.RawMessage, requested, path string) (string, map[string]json.RawMessage, bool, error) {
	paths, err := objectField(setting, "paths", path)
	if err != nil {
		return "", nil, false, err
	}
	profilesRaw, exists := paths["profiles"]
	if !exists {
		if _, activeExists := paths["active_profile"]; activeExists {
			return "", nil, false, configError("%s: paths.active_profile requires paths.profiles", path)
		}
		if requested != "" {
			return "", nil, false, configError("%s: profile '%s' was requested but the session configures no paths.profiles", path, requested)
		}
		return "", nil, false, nil
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(profilesRaw, &profiles); err != nil || len(profiles) == 0 {
		return "", nil, false, configError("%s: paths.profiles must be a non-empty object", path)
	}
	active, err := requiredStringField(paths, "active_profile", path+": paths")
	if err != nil {
		return "", nil, false, err
	}
	if !profileNamePattern.MatchString(active) {
		return "", nil, false, configError("%s: paths.active_profile must contain only letters, numbers, underscores, or hyphens", path)
	}
	selected := requested
	if selected == "" {
		selected = active
	}
	if !profileNamePattern.MatchString(selected) {
		return "", nil, false, configError("%s: path profile '%s' must contain only letters, numbers, underscores, or hyphens", path, selected)
	}
	raw, exists := profiles[selected]
	if !exists {
		return "", nil, false, configError("%s: path profile '%s' is not defined in paths.profiles", path, selected)
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(raw, &profile); err != nil || profile == nil {
		return "", nil, false, configError("%s: paths.profiles.%s must be an object", path, selected)
	}
	names, err := orderedObjectKeys(profilesRaw)
	if err != nil {
		return "", nil, false, configError("%s: paths.profiles must be a non-empty object", path)
	}
	for _, name := range names {
		if !profileNamePattern.MatchString(name) {
			return "", nil, false, configError("%s: invalid paths.profiles key '%s'", path, name)
		}
		if err := validateProfileFields(name, profiles[name], path); err != nil {
			return "", nil, false, err
		}
	}
	return selected, profile, true, nil
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

func keepBackupTurns(raw json.RawMessage, path string) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return defaultKeepBackupTurns, nil
	}
	var compaction map[string]json.RawMessage
	if err := json.Unmarshal(raw, &compaction); err != nil || compaction == nil {
		return 0, configError("%s: compaction must be an object", path)
	}
	keepbackupRaw, exists := compaction["keepbackup"]
	if !exists || string(keepbackupRaw) == "null" {
		return defaultKeepBackupTurns, nil
	}
	var keepbackup map[string]json.RawMessage
	if err := json.Unmarshal(keepbackupRaw, &keepbackup); err != nil || keepbackup == nil {
		return 0, configError("%s: compaction.keepbackup must be an object", path)
	}
	turnsRaw, exists := keepbackup["turns"]
	if !exists || string(turnsRaw) == "null" {
		return defaultKeepBackupTurns, nil
	}
	var turns int
	if err := json.Unmarshal(turnsRaw, &turns); err != nil || turns < 0 || turns > maxKeepBackupTurns {
		return 0, configError("%s: compaction.keepbackup.turns must be an integer between 0 and %d", path, maxKeepBackupTurns)
	}
	return turns, nil
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
	authMode, err := stringField(fields, "auth_mode", "api_key", path)
	if err != nil {
		return Model{}, err
	}
	if authMode != "api_key" && authMode != "openai_oauth" {
		return Model{}, configError("%s: auth_mode must be api_key or openai_oauth", path)
	}
	if authMode == "api_key" {
		if _, exists := fields["oauth_auth_file"]; exists {
			return Model{}, configError("%s: oauth_auth_file requires auth_mode openai_oauth", path)
		}
	}
	oauthAuthFile := ""
	if authMode == "openai_oauth" {
		// base_url means the same here as in api_key mode: the root the Responses path is
		// appended to. It is how a proxy is put in front of the endpoint.
		if baseURL == "" {
			baseURL = defaultOpenAIOAuthBaseURL
		}
		oauthAuthFile, err = oauthAuthFilePath(fields, path)
		if err != nil {
			return Model{}, err
		}
	}
	apiKeyEnvDefault := "OPENAI_API_KEY"
	if strings.EqualFold(provider, "openrouter") || strings.Contains(baseURL, "openrouter.ai") {
		apiKeyEnvDefault = "OPENROUTER_API_KEY"
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
	return Model{
		Provider:      provider,
		Name:          modelName,
		APIKeyEnv:     apiKeyEnv,
		BaseURL:       baseURL,
		AuthMode:      authMode,
		OAuthAuthFile: oauthAuthFile,
		Request:       request,
		HostedTools:   hostedTools,
		PromptRole:    promptRole,
		PromptText:    promptText,
	}, nil
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

// defaultOAuthAuthFile is the credential file used when model.json names none, relative
// to the home directory.
const defaultOAuthAuthFile = ".jad/auth/openai-codex.json"

// oauthAuthFilePath resolves oauth_auth_file: absent means the default file in the home
// directory; a given value must be absolute or start with "~/" so the file never depends
// on the working directory. A null value is an error, not a quiet default.
func oauthAuthFilePath(fields map[string]json.RawMessage, path string) (string, error) {
	raw, exists := fields["oauth_auth_file"]
	if !exists {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", configError("%s: oauth_auth_file is not set and the home directory is unknown: %v", path, err)
		}
		return filepath.Join(home, filepath.FromSlash(defaultOAuthAuthFile)), nil
	}
	var configured string
	if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
		return "", configError("%s: oauth_auth_file must be a non-empty string", path)
	}
	expanded := expandHome(strings.TrimSpace(configured))
	if !filepath.IsAbs(expanded) {
		return "", configError("%s: oauth_auth_file must be absolute or start with ~", path)
	}
	return filepath.Clean(expanded), nil
}

func loadPrompt(setting, profile map[string]json.RawMessage, profileConfigured bool, cwd, path string) (string, string, error) {
	role := "system"
	if raw, ok := setting["prompt_role"]; ok {
		if err := json.Unmarshal(raw, &role); err != nil || (role != "system" && role != "developer") {
			return "", "", configError("%s: prompt_role must be system or developer", path)
		}
	}
	var documents []string
	if raw, ok := setting["prompt_documents"]; ok {
		if err := json.Unmarshal(raw, &documents); err != nil || documents == nil {
			return "", "", configError("%s: prompt_documents must be an array", path)
		}
	}
	if profileConfigured {
		if raw, ok := profile["prompt_documents"]; ok {
			var profileDocuments []string
			if err := json.Unmarshal(raw, &profileDocuments); err != nil || profileDocuments == nil {
				return "", "", configError("%s: paths.profiles prompt_documents must be an array", path)
			}
			documents = append(documents, profileDocuments...)
		}
	}
	texts := make([]string, 0, len(documents)+1)
	if profileConfigured {
		if raw, ok := profile["instructions"]; ok {
			text, valid := nonEmptyJSONString(raw)
			if !valid {
				return "", "", configError("%s: paths.profiles instructions must be a non-empty string", path)
			}
			texts = append(texts, text)
		}
	}
	for index, document := range documents {
		contents, err := readPromptDocument(document, index, "prompt_documents", cwd, path)
		if err != nil {
			return "", "", err
		}
		texts = append(texts, contents)
	}
	return role, strings.Join(texts, "\n\n"), nil
}

// readPromptDocument reads one prompt document: ~ is expanded and a relative path
// resolves from cwd. field names the setting in errors.
func readPromptDocument(document string, index int, field, cwd, path string) (string, error) {
	if strings.TrimSpace(document) == "" {
		return "", configError("%s: %s[%d] must be a non-empty string", path, field, index)
	}
	resolved := expandHome(document)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cwd, resolved)
	}
	contents, err := os.ReadFile(filepath.Clean(resolved))
	if err != nil {
		return "", configError("%s: %s[%d] could not be read: %s", path, field, index, resolved)
	}
	return string(contents), nil
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
	contents, err := os.ReadFile(path)
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
