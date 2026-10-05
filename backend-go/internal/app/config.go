// Package app composes filesystem session resolution, HTTP/SSE routes, and the
// provider adapter into the executable Go backend.
package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"example.com/agent-harness/backend-go/internal/contentlibrary"
	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

const apiTokenEnvironmentName = "AGENT_API_TOKEN"

// Config contains all runtime dependencies and settings loaded before serving.
type Config struct {
	Host               string
	Port               int
	Roots              []sessionfs.Root
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
		Host:  host,
		Port:  port,
		Roots: append([]sessionfs.Root(nil), loaded.Roots...),
		HTTP: httpapi.Config{
			APIToken:                   environment[apiTokenEnvironmentName],
			AllowedOrigins:             append([]string(nil), loaded.Server.AllowedOrigins...),
			MaxBodyBytes:               loaded.Server.MaxBodyBytes,
			SSEMaxBufferedBytes:        loaded.Server.SSEMaxBufferedBytes,
			RunStateRetention:          loaded.Server.RunStateRetention,
			AllowZeroRunStateRetention: loaded.Server.AllowZeroRunStateRetention,
			ToolStreamInterval:         loaded.Server.ToolStreamInterval,
			ConditionalRequests:        loaded.Server.ConditionalRequests,
			SnapshotTurns:              loaded.Server.SnapshotTurns,
			MaxPageTurns:               loaded.Server.MaxPageTurns,
			MaxInlinePartBytes:         loaded.Server.MaxInlinePartBytes,
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
