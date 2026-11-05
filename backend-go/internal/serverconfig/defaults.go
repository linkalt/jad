// Package serverconfig contains defaults and bounds shared by the root-setting
// loader and HTTP adapter. It has no dependencies on either layer.
package serverconfig

const (
	DefaultToolStreamIntervalMS    = 120
	MaxToolStreamIntervalMS        = 60 * 1000
	DefaultSSEHeartbeatIntervalMS  = 5_000
	MaxSSEHeartbeatIntervalMS      = 5 * 60 * 1000
	DefaultSSEWriteTimeoutMS       = 30_000
	MaxSSEWriteTimeoutMS           = 5 * 60 * 1000
	DefaultReadHeaderTimeoutMS     = 60_000
	MaxReadHeaderTimeoutMS         = 5 * 60 * 1000
	DefaultIdleTimeoutMS           = 5_000
	DefaultSessionDeleteStopWaitMS = 10_000
	MaxSessionDeleteStopWaitMS     = 5 * 60 * 1000
	// server.session_store_idle_ms: how long an unused session's transcript store stays
	// cached; zero keeps it until the session is deleted or the server stops.
	DefaultSessionStoreIdleMS            = 10 * 60 * 1000
	MaxSessionStoreIdleMS                = 24 * 60 * 60 * 1000
	MaxIdleTimeoutMS                     = 60 * 60 * 1000
	DefaultRunContentMemoryBytes         = 100 * 1024
	DefaultRunContentWriteBatchBytes     = 32 * 1024
	DefaultRunContentFlushIntervalMS     = 25
	DefaultRunContentMaxIndexSegments    = 1024
	DefaultProviderMemoryTraceIntervalMS = 250

	MaxRunContentMemoryBytes = 16 * 1024 * 1024

	// The in-memory fallback of the run content cache (server.run_content_fallback_max_bytes):
	// once the partial file cannot be used, the streamed text is kept in memory up to this
	// many bytes and the late-subscriber snapshot is cut there.
	DefaultRunContentFallbackMaxBytes = 64 * 1024 * 1024
	MinRunContentFallbackMaxBytes     = 1024
	MaxRunContentFallbackMaxBytes     = 1024 * 1024 * 1024
	MaxRunContentFlushIntervalMS      = 60 * 1000
	MaxRunContentIndexSegments        = 65_536
)

// server.fork_name_max_bytes: the longest name (and display name) a fork into a new profile
// may give it. The name becomes a file name with ".json", so the bound stays under the 255
// bytes most file systems allow.
const (
	DefaultForkNameMaxBytes = 200
	MaxForkNameMaxBytes     = 250
)

// server.steer_max_pending: the most steering messages one run holds before it stores
// them; zero turns POST /run/steer off.
const (
	DefaultSteerMaxPending = 16
	MaxSteerMaxPending     = 1000
)

// Images in a run request (server.max_request_images, server.max_request_image_bytes).
const (
	DefaultMaxRequestImages     = 8
	MaxMaxRequestImages         = 16
	DefaultMaxRequestImageBytes = 5 * 1024 * 1024
	MaxMaxRequestImageBytes     = 25 * 1024 * 1024
)

// Files in a run request (server.max_request_files, server.max_request_file_bytes).
const (
	DefaultMaxRequestFiles     = 4
	MaxMaxRequestFiles         = 8
	DefaultMaxRequestFileBytes = 10 * 1024 * 1024
	MaxMaxRequestFileBytes     = 25 * 1024 * 1024
)

// Runs at the same time (server.max_concurrent_runs). 0 means no cap.
const (
	DefaultMaxConcurrentRuns = 0
	MaxMaxConcurrentRuns     = 1024
	// RunCapRetryAfterSeconds is the Retry-After of a run start refused by the cap.
	RunCapRetryAfterSeconds = 5
)

// Leaf tool calls that may run at once in the process (server.max_leaf_calls). 256 is four
// full batches of the largest per-response limit (64), so a few runs overlap at full fan-out
// while the process stays bounded.
const (
	DefaultMaxLeafCalls = 256
	MaxMaxLeafCalls     = 1024
)

// How long shutdown waits for in-flight requests before it closes their connections
// (server.shutdown_timeout_ms). 0 waits without a limit.
const (
	DefaultShutdownTimeoutMS = 10_000
	MaxShutdownTimeoutMS     = 5 * 60 * 1000
)

// DefaultRecoverProcessesAtStart is the default of server.recover_processes_at_start.
const DefaultRecoverProcessesAtStart = true

// DefaultRecoverSubagentRunsAtStart is the default of server.recover_subagent_runs_at_start.
const DefaultRecoverSubagentRunsAtStart = true

// Logging defaults and the accepted values of the server.log_level and
// server.log_format settings.
const (
	DefaultLogLevel  = "info"
	DefaultLogFormat = "text"
)

// IsLogLevel reports whether value is an accepted server.log_level.
func IsLogLevel(value string) bool {
	switch value {
	case "debug", "info", "warn", "error", "off":
		return true
	}
	return false
}

// IsLogFormat reports whether value is an accepted server.log_format.
func IsLogFormat(value string) bool {
	return value == "text" || value == "json"
}
