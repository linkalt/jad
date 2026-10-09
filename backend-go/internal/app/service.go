package app

import (
	"context"
	"net/http"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/contentlibrary"
	"example.com/agent-harness/backend-go/internal/httpapi"
)

// Service owns the HTTP adapter and filesystem resolver for one server
// instance.
type Service struct {
	Handler  http.Handler
	api      *httpapi.Handler
	resolver *Resolver

	// stopRecovery cancels the start-time storage recovery; recovering closes when
	// that scan has ended. Both are nil when the scan was not started.
	stopRecovery context.CancelFunc
	recovering   chan struct{}
	closeOnce    sync.Once
}

// NewService wires validated runtime configuration into the injectable HTTP
// handler and filesystem-backed session resolver.
func NewService(config Config) (*Service, error) {
	resolver, err := NewResolver(config.Roots, config.Environment, CatalogConfig{
		SnapshotTurns:      config.HTTP.SnapshotTurns,
		MaxPageTurns:       config.HTTP.MaxPageTurns,
		MaxInlinePartBytes: config.HTTP.MaxInlinePartBytes,
	}, WithLogger(config.HTTP.Logger))
	if err != nil {
		return nil, err
	}
	api, err := httpapi.NewHandlerWithRootLibrary(config.HTTP, resolver, contentlibrary.New(config.RootLibraryOptions))
	if err != nil {
		resolver.Close()
		return nil, err
	}
	service := &Service{Handler: api, api: api, resolver: resolver}
	options := StorageRecoveryOptions{Processes: config.RecoverProcessesAtStart, SubagentRuns: config.RecoverSubagentRunsAtStart}
	if options.Processes || options.SubagentRuns {
		service.startStorageRecovery(options)
	}
	return service, nil
}

// startStorageRecovery recovers the storages options names, for every session, in the
// background, so listening is not delayed by it.
func (s *Service) startStorageRecovery(options StorageRecoveryOptions) {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopRecovery = cancel
	s.recovering = make(chan struct{})
	go func() {
		defer close(s.recovering)
		began := time.Now()
		report, err := s.resolver.RecoverStorages(ctx, options)
		logger := s.resolver.logger
		switch {
		case err != nil && ctx.Err() != nil:
			logger.Info("storage recovery at start was cancelled")
		case err != nil:
			logger.Warn("storage recovery at start failed", "error", err)
		default:
			logger.Info("storage recovery at start finished",
				"processes", options.Processes, "subagent_runs", options.SubagentRuns,
				"sessions", report.Sessions, "process_storages", report.Storages, "run_storages", report.RunStorages,
				"unreadable", report.Unreadable, "failed", report.Failed, "took", time.Since(began).String())
		}
	}()
}

// Close cancels active runs before preventing additional filesystem resolutions.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		// The scan ends first: it builds managers the resolver's Close would stop.
		if s.stopRecovery != nil {
			s.stopRecovery()
			<-s.recovering
		}
	})
	if s.api != nil {
		s.api.Close()
	}
	if s.resolver != nil {
		s.resolver.Close()
	}
}
