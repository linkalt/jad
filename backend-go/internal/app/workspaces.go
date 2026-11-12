package app

import (
	"context"

	"jad/backend-go/internal/httpapi"
)

// Workspaces lists the workspaces of the workspace layout for /api/workspaces: every listed
// one, served or skipped (with why), the start workspace first. Outside the workspace layout
// the list is empty. The local admin API sees every workspace by design (design §6).
func (r *Resolver) Workspaces(ctx context.Context) ([]httpapi.WorkspaceInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	infos := make([]httpapi.WorkspaceInfo, 0, len(r.workspaces))
	for _, workspace := range r.workspaces {
		infos = append(infos, httpapi.WorkspaceInfo{ID: workspace.ID, Label: workspace.Label, Directory: workspace.Directory, Start: workspace.Start, Unavailable: workspace.Unavailable})
	}
	return infos, nil
}
