package app

import (
	"log/slog"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type resolverOptions struct {
	logger *slog.Logger
	// leafBudget is the budget of leaf tool calls the resolver shares with its runs. Nil
	// means the process default, which NewResolver creates.
	leafBudget *tools.LeafBudget
	// workspaces are the workspaces of the workspace layout, served or skipped, for
	// /api/workspaces; nil outside it.
	workspaces []sessionfs.Workspace
}

// ResolverOption customizes NewResolver.
type ResolverOption func(*resolverOptions)

// withLeafBudget sets the budget of leaf tool calls. Only the process default and tests
// that bound the leaf calls to one token use it, so it stays unexported.
func withLeafBudget(budget *tools.LeafBudget) ResolverOption {
	return func(options *resolverOptions) { options.leafBudget = budget }
}

// WithLogger sets the logger for the resolver and the stores and adapters it
// builds. A nil logger, like no option, discards their records.
func WithLogger(logger *slog.Logger) ResolverOption {
	return func(options *resolverOptions) { options.logger = logging.Or(logger) }
}

// WithWorkspaces sets the workspaces /api/workspaces lists (sessionfs.Config.Workspaces).
func WithWorkspaces(workspaces []sessionfs.Workspace) ResolverOption {
	return func(options *resolverOptions) {
		options.workspaces = append([]sessionfs.Workspace(nil), workspaces...)
	}
}
