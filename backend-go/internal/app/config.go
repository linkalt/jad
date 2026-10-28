// Package app composes filesystem session resolution, HTTP/SSE routes, and the
// provider adapter into the executable Go backend.
package app

import (
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	// RemoteAPI is server.remote_api.
	RemoteAPI          sessionfs.RemoteAPISettings
	HTTP               httpapi.Config
	Environment        map[string]string
	RootLibraryOptions contentlibrary.Options
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
	config := Config{
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
		RemoteAPI:                  loaded.Server.RemoteAPI,
		HTTP: httpapi.Config{
			APIToken:                   environment[apiTokenEnvironmentName],
			AllowedOrigins:             append([]string(nil), loaded.Server.AllowedOrigins...),
			MaxBodyBytes:               loaded.Server.MaxBodyBytes,
			SSEMaxBufferedBytes:        loaded.Server.SSEMaxBufferedBytes,
			SSEHeartbeatInterval:       loaded.Server.SSEHeartbeatInterval,
			SSEWriteTimeout:            loaded.Server.SSEWriteTimeout,
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
			RunContentFallbackMaxBytes: loaded.Server.RunContentFallbackMaxBytes,
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
	}
	if err := config.checkRemoteListenHost(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// listenHostPattern is a plain hostname: dot-separated labels of letters, digits, '-' and '_'.
// net.Listen resolves anything else (a URL, a host:port, stray punctuation) unpredictably, so
// such a server.host is refused rather than left to the resolver.
var listenHostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9_-]*[A-Za-z0-9])?)*$`)

// listensOnEveryInterface reports whether net.Listen would bind every interface for host: the
// empty host, an unspecified address in any spelling (0.0.0.0, ::, ::0, 0:0:0:0:0:0:0:0,
// ::ffff:0.0.0.0, bracketed or with a zone), or a value that is neither an address nor a plain
// hostname (numeric spellings such as 0, 0.0, 00, 0x0 are not hostnames). A hostname resolving to such an address is out of scope: it is a name the operator
// wrote, not a wildcard spelling.
func listensOnEveryInterface(host string) bool {
	host = strings.TrimSpace(host)
	if inner, ok := strings.CutPrefix(host, "["); ok {
		host, _ = strings.CutSuffix(inner, "]")
	}
	if host == "" {
		return true
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.WithZone("").Unmap().IsUnspecified()
	}
	if !listenHostPattern.MatchString(host) {
		return true
	}
	// The cgo resolver reads 0, 0.0, 00, 0x0 as 0.0.0.0 (inet_aton): a name needs a letter and a last label that is not a number.
	last := host[strings.LastIndexByte(host, '.')+1:]
	return !strings.ContainsFunc(host, unicode.IsLetter) || listenHostNumericLabel.MatchString(last)
}

var listenHostNumericLabel = regexp.MustCompile(`^(?:[0-9]+|0[xX][0-9A-Fa-f]*)$`)

// checkRemoteListenHost refuses a wildcard listen address when the remote API answers on the
// TCP listener: the bearer token of every remote would then answer on every interface of the
// machine, in clear text. A listen_unix server never does, so any host is fine there. The
// default host is loopback; reaching the API from another machine means naming one address, the
// VPN address, in server.host or HOST.
func (c Config) checkRemoteListenHost() error {
	if !c.RemoteAPI.Enabled || c.RemoteAPI.ListenUnix != "" {
		return nil
	}
	if listensOnEveryInterface(c.Host) {
		return fmt.Errorf("server.host must name the one address the remote API is reached at, the default 127.0.0.1 or the VPN address, when server.remote_api is enabled: a wildcard would answer on every interface, and the bearer token travels in clear text")
	}
	return nil
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
