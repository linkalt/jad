package app

import (
	"net/http"

	"example.com/agent-harness/backend-go/internal/contentlibrary"
	"example.com/agent-harness/backend-go/internal/httpapi"
)

// Service owns the HTTP adapter and filesystem resolver for one server
// instance.
type Service struct {
	Handler  http.Handler
	api      *httpapi.Handler
	resolver *Resolver
}

// NewService wires validated runtime configuration into the injectable HTTP
// handler and filesystem-backed session resolver.
func NewService(config Config) (*Service, error) {
	resolver, err := NewResolver(config.Roots, config.Environment, CatalogConfig{
		SnapshotTurns:      config.HTTP.SnapshotTurns,
		MaxPageTurns:       config.HTTP.MaxPageTurns,
		MaxInlinePartBytes: config.HTTP.MaxInlinePartBytes,
	})
	if err != nil {
		return nil, err
	}
	api, err := httpapi.NewHandlerWithRootLibrary(config.HTTP, resolver, contentlibrary.New(config.RootLibraryOptions))
	if err != nil {
		resolver.Close()
		return nil, err
	}
	return &Service{Handler: api, api: api, resolver: resolver}, nil
}

// Close cancels active runs before preventing additional filesystem resolutions.
func (s *Service) Close() {
	if s == nil {
		return
	}
	if s.api != nil {
		s.api.Close()
	}
	if s.resolver != nil {
		s.resolver.Close()
	}
}
