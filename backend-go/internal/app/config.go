// Package app composes filesystem session resolution, HTTP/SSE routes, and the
// provider adapter into the executable Go backend.
package app

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"jad/backend-go/internal/contentlibrary"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
)

const apiTokenEnvironmentName = "JAD_API_TOKEN"
const legacyAPITokenEnvironmentName = "AGENT_API_TOKEN"

// Config contains all runtime dependencies and settings loaded before serving.
type Config struct {
	Host string
	Port int
	// ReadHeaderTimeout and IdleTimeout bound slow or idle clients; zero
	// disables a limit. There is deliberately no read or write timeout, which
	// would cut long-lived SSE run streams.
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	// ShutdownTimeout bounds how long ShutdownHTTPServer waits for in-flight requests;
	// zero waits without a limit.
	ShutdownTimeout time.Duration
	// Log configures the structured logger built by main; see internal/logging.
	Log logging.Settings
	// RecoverProcessesAtStart starts a background recovery of every session's managed
	// processes when the service is created; see Resolver.RecoverStorages.
	RecoverProcessesAtStart bool
	// RecoverSubagentRunsAtStart does the same for the sub-agent run manifests.
	RecoverSubagentRunsAtStart bool
	Roots                      []sessionfs.Root
	HTTP                       httpapi.Config
	Environment                map[string]string
	RootLibraryOptions         contentlibrary.Options
}

// LoadConfig reads root settings and server environment overrides. A supplied
// environment map makes configuration deterministic in tests and embedders.
func LoadConfig(options sessionfs.LoadOptions) (Config, error) {
	environment := options.Environment
	if environment == nil {
		environment = currentEnvironment()
	}
	options.Environment = environment
	if _, hasNew := environment[apiTokenEnvironmentName]; !hasNew {
		if _, hasOld := environment[legacyAPITokenEnvironmentName]; hasOld {
			return Config{}, fmt.Errorf("%s is no longer read; set %s instead", legacyAPITokenEnvironmentName, apiTokenEnvironmentName)
		}
	}
	loaded, err := sessionfs.Load(options)
	if err != nil {
		return Config{}, err
	}

	host := loaded.Server.Host
	if value, exists := environment["HOST"]; exists {
		host = strings.TrimSpace(value)
	}
	port := loaded.Server.Port
	if value, exists := environment["PORT"]; exists {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 0 || parsed > 65535 {
			return Config{}, fmt.Errorf("PORT must be an integer between 0 and 65535")
		}
		port = parsed
	}
	resolverEnvironment := cloneEnvironment(environment)
	delete(resolverEnvironment, apiTokenEnvironmentName)
	return Config{
		Host:              host,
		Port:              port,
		ReadHeaderTimeout: loaded.Server.ReadHeaderTimeout,
		IdleTimeout:       loaded.Server.IdleTimeout,
		ShutdownTimeout:   loaded.Server.ShutdownTimeout,
		Log: logging.Settings{
			Level:  loaded.Server.LogLevel,
			Format: loaded.Server.LogFormat,
			File:   loaded.Server.LogFile,
		},
		RecoverProcessesAtStart:    loaded.Server.RecoverProcessesAtStart,
		RecoverSubagentRunsAtStart: loaded.Server.RecoverSubagentRunsAtStart,
		Roots:                      append([]sessionfs.Root(nil), loaded.Roots...),
		HTTP: httpapi.Config{
			APIToken:                   environment[apiTokenEnvironmentName],
			AllowedOrigins:             append([]string(nil), loaded.Server.AllowedOrigins...),
			MaxBodyBytes:               loaded.Server.MaxBodyBytes,
			SSEMaxBufferedBytes:        loaded.Server.SSEMaxBufferedBytes,
			SSEHeartbeatInterval:       loaded.Server.SSEHeartbeatInterval,
			SessionDeleteStopWait:      loaded.Server.SessionDeleteStopWait,
			RunStateRetention:          loaded.Server.RunStateRetention,
			AllowZeroRunStateRetention: loaded.Server.AllowZeroRunStateRetention,
			ToolStreamInterval:         loaded.Server.ToolStreamInterval,
			ConditionalRequests:        loaded.Server.ConditionalRequests,
			SnapshotTurns:              loaded.Server.SnapshotTurns,
			MaxPageTurns:               loaded.Server.MaxPageTurns,
			MaxInlinePartBytes:         loaded.Server.MaxInlinePartBytes,
			MaxRequestImages:           loaded.Server.MaxRequestImages,
			MaxRequestImageBytes:       loaded.Server.MaxRequestImageBytes,
			MaxRequestFiles:            loaded.Server.MaxRequestFiles,
			MaxRequestFileBytes:        loaded.Server.MaxRequestFileBytes,
			MaxConcurrentRuns:          loaded.Server.MaxConcurrentRuns,
			RunContentMemoryBytes:      loaded.Server.RunContentMemoryBytes,
			RunContentWriteBatchBytes:  loaded.Server.RunContentWriteBatchBytes,
			RunContentFlushInterval:    loaded.Server.RunContentFlushInterval,
			RunContentMaxIndexSegments: loaded.Server.RunContentMaxIndexSegments,
		},
		Environment: resolverEnvironment,
		RootLibraryOptions: contentlibrary.Options{
			WorkingDir:      options.WorkingDir,
			RootSettingPath: options.RootSettingPath,
			SessionsDir:     options.SessionsDir,
			Environment:     cloneEnvironment(resolverEnvironment),
		},
	}, nil
}

func currentEnvironment() map[string]string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func cloneEnvironment(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

// NewHTTPServer returns an http.Server for handler with the configured header
// and idle timeouts. It sets no ReadTimeout or WriteTimeout because those would
// end long-lived SSE run streams.
func (c Config) NewHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
}
