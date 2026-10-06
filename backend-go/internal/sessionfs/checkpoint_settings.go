package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	// CheckpointSnapshotFirstMutation snapshots just before the first call that can
	// change files; CheckpointSnapshotRunStart snapshots at the start of every run.
	CheckpointSnapshotFirstMutation = "first_mutation"
	CheckpointSnapshotRunStart      = "run_start"

	// The defaults and bounds mirror TypeScript's DEFAULT_CHECKPOINT_* and
	// MAX_CHECKPOINT_* constants. The file and byte limits are placeholders until
	// they are measured on a large workspace (design_checkpoint.md, section 6).
	defaultCheckpointGitBinary      = "git"
	defaultCheckpointMaxCheckpoints = 20
	maxCheckpointMaxCheckpoints     = 1000
	defaultCheckpointMaxFiles       = 20_000
	maxCheckpointMaxFiles           = 1_000_000
	defaultCheckpointMaxTotalBytes  = 512 * 1024 * 1024
	maxCheckpointMaxTotalBytes      = int64(1) << 40
	defaultCheckpointTimeoutMS      = 120_000
	minCheckpointTimeoutMS          = 1_000
	maxCheckpointTimeoutMS          = 3_600_000
)

var checkpointKeys = []string{
	"enabled", "git_binary", "include_mcp_tools", "max_checkpoints", "max_files",
	"max_total_bytes", "safe_restore", "snapshot_on", "timeout_ms",
}

// CheckpointSettings controls run checkpoints: a private git directory that
// records the workspace before a run changes it and when the run ends.
type CheckpointSettings struct {
	// Enabled is false by default; nothing is snapshotted and no files are created while it is off.
	Enabled bool
	// GitBinary is the git executable the snapshots run.
	GitBinary string
	// MaxCheckpoints is how many checkpoints are kept; the oldest are pruned first.
	MaxCheckpoints int64
	// MaxFiles skips a snapshot of a workspace with more files than this.
	MaxFiles int64
	// MaxTotalBytes skips a snapshot once the private repository is larger than this.
	MaxTotalBytes int64
	// TimeoutMS is the longest one git command may run before the snapshot is abandoned.
	TimeoutMS int64
	// IncludeMCPTools treats a call to any MCP tool as a change to the workspace.
	IncludeMCPTools bool
	// SnapshotOn is "first_mutation" or "run_start".
	SnapshotOn string
	// SafeRestore makes "safe" the default restore mode.
	SafeRestore bool
}

// normalizeCheckpointSettings reads the `checkpoint` node. Only an absent key takes
// a default: a null or misspelt value must not quietly pick one, so a typo cannot
// turn undo off without an error.
func normalizeCheckpointSettings(raw json.RawMessage, settingPath string) (CheckpointSettings, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		decoded, err := decodeConfigObject(raw, settingPath+": checkpoint")
		if err != nil {
			return CheckpointSettings{}, configError("%s: checkpoint must be an object", settingPath)
		}
		fields = decoded
	}
	for key := range fields {
		if !containsString(checkpointKeys, key) {
			return CheckpointSettings{}, unsupportedToolKey(settingPath+": checkpoint", key, checkpointKeys, "")
		}
	}
	flag := func(name string, fallback bool) (bool, error) {
		value, exists := fields[name]
		if !exists {
			return fallback, nil
		}
		var configured bool
		if err := json.Unmarshal(value, &configured); err != nil || strings.TrimSpace(string(value)) == "null" {
			return false, configError("%s: checkpoint.%s must be a boolean", settingPath, name)
		}
		return configured, nil
	}
	bounded := func(name string, fallback, minimum, maximum int64) (int64, error) {
		value, exists := fields[name]
		if !exists {
			return fallback, nil
		}
		configured, ok := configSafeInteger(value)
		if !ok || configured < minimum || configured > maximum {
			return 0, configError("%s: checkpoint.%s must be a safe integer between %d and %d", settingPath, name, minimum, maximum)
		}
		return configured, nil
	}
	var settings CheckpointSettings
	var err error
	if settings.Enabled, err = flag("enabled", false); err != nil {
		return CheckpointSettings{}, err
	}
	settings.GitBinary = defaultCheckpointGitBinary
	if value, exists := fields["git_binary"]; exists {
		var binary string
		if json.Unmarshal(value, &binary) != nil || strings.TrimSpace(binary) == "" {
			return CheckpointSettings{}, configError("%s: checkpoint.git_binary must be a non-empty string", settingPath)
		}
		settings.GitBinary = strings.TrimSpace(binary)
	}
	if settings.MaxCheckpoints, err = bounded("max_checkpoints", defaultCheckpointMaxCheckpoints, 1, maxCheckpointMaxCheckpoints); err != nil {
		return CheckpointSettings{}, err
	}
	if settings.MaxFiles, err = bounded("max_files", defaultCheckpointMaxFiles, 1, maxCheckpointMaxFiles); err != nil {
		return CheckpointSettings{}, err
	}
	if settings.MaxTotalBytes, err = bounded("max_total_bytes", defaultCheckpointMaxTotalBytes, 1, maxCheckpointMaxTotalBytes); err != nil {
		return CheckpointSettings{}, err
	}
	if settings.TimeoutMS, err = bounded("timeout_ms", defaultCheckpointTimeoutMS, minCheckpointTimeoutMS, maxCheckpointTimeoutMS); err != nil {
		return CheckpointSettings{}, err
	}
	if settings.IncludeMCPTools, err = flag("include_mcp_tools", true); err != nil {
		return CheckpointSettings{}, err
	}
	settings.SnapshotOn = CheckpointSnapshotFirstMutation
	if value, exists := fields["snapshot_on"]; exists {
		var snapshotOn string
		if json.Unmarshal(value, &snapshotOn) != nil || (snapshotOn != CheckpointSnapshotFirstMutation && snapshotOn != CheckpointSnapshotRunStart) {
			return CheckpointSettings{}, configError("%s: checkpoint.snapshot_on must be \"first_mutation\" or \"run_start\"", settingPath)
		}
		settings.SnapshotOn = snapshotOn
	}
	if settings.SafeRestore, err = flag("safe_restore", true); err != nil {
		return CheckpointSettings{}, err
	}
	return settings, nil
}
