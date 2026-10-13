package app

import (
	"log/slog"

	"jad/backend-go/internal/logging"
)

type resolverOptions struct {
	logger *slog.Logger
}

// ResolverOption customizes NewResolver.
type ResolverOption func(*resolverOptions)

// WithLogger sets the logger for the resolver and the stores and adapters it
// builds. A nil logger, like no option, discards their records.
func WithLogger(logger *slog.Logger) ResolverOption {
	return func(options *resolverOptions) { options.logger = logging.Or(logger) }
}
