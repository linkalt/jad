// Package serverconfig contains defaults and bounds shared by the root-setting
// loader and HTTP adapter. It has no dependencies on either layer.
package serverconfig

const (
	DefaultToolStreamIntervalMS          = 120
	MaxToolStreamIntervalMS              = 60 * 1000
	DefaultSSEHeartbeatIntervalMS        = 5_000
	MaxSSEHeartbeatIntervalMS            = 5 * 60 * 1000
	DefaultReadHeaderTimeoutMS           = 60_000
	MaxReadHeaderTimeoutMS               = 5 * 60 * 1000
	DefaultIdleTimeoutMS                 = 5_000
	MaxIdleTimeoutMS                     = 60 * 60 * 1000
	DefaultRunContentMemoryBytes         = 100 * 1024
	DefaultRunContentWriteBatchBytes     = 32 * 1024
	DefaultRunContentFlushIntervalMS     = 25
	DefaultRunContentMaxIndexSegments    = 1024
	DefaultProviderMemoryTraceIntervalMS = 250

	MaxRunContentMemoryBytes     = 16 * 1024 * 1024
	MaxRunContentFlushIntervalMS = 60 * 1000
	MaxRunContentIndexSegments   = 65_536
)

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
