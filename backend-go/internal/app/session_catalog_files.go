package app

import (
	"context"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

func (r *Resolver) ListSessionConfigs(ctx context.Context) ([]httpapi.CatalogFileInfo, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return nil, err
	}
	files, err := sessionfs.ListSessionFiles(r.roots)
	if err != nil {
		return nil, mapCatalogFileError(err)
	}
	listing := make([]httpapi.CatalogFileInfo, len(files))
	for index, file := range files {
		listing[index] = httpapi.CatalogFileInfo{ID: file.ID, ETag: file.ETag, Name: file.Name, Category: file.Category}
	}
	return listing, nil
}

func (r *Resolver) ReadSessionConfig(ctx context.Context, reference string) (httpapi.CatalogFile, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.CatalogFile{}, err
	}
	file, err := sessionfs.ReadSessionFile(r.roots, reference)
	if err != nil {
		return httpapi.CatalogFile{}, mapCatalogFileError(err)
	}
	return httpapi.CatalogFile{CatalogFileInfo: httpapi.CatalogFileInfo{ID: file.ID, ETag: file.ETag}, Data: file.Data}, nil
}

func (r *Resolver) WriteSessionConfig(ctx context.Context, reference string, data []byte, ifMatch, ifNoneMatch string) (httpapi.CatalogFileInfo, bool, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.CatalogFileInfo{}, false, err
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	info, created, err := sessionfs.WriteSessionFile(r.roots, reference, data, sessionfs.WriteCondition{IfMatch: ifMatch, IfNoneMatch: ifNoneMatch}, true)
	if err != nil {
		return httpapi.CatalogFileInfo{}, false, mapCatalogFileError(err)
	}
	return httpapi.CatalogFileInfo{ID: info.ID, ETag: info.ETag}, created, nil
}

func (r *Resolver) CheckSessionConfigDelete(ctx context.Context, reference, ifMatch string) (string, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return "", err
	}
	location, err := sessionfs.CheckSessionDelete(r.roots, reference, ifMatch)
	if err != nil {
		return "", mapCatalogFileError(err)
	}
	return location.Reference, nil
}

func (r *Resolver) DeleteSessionConfig(ctx context.Context, reference, ifMatch string, deleteState bool) error {
	if err := requireSettingsContext(ctx); err != nil {
		return err
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	stateDir, err := sessionfs.DeleteSessionFile(r.roots, reference, ifMatch, deleteState)
	if stateDir != "" {
		r.forgetStores(stateDir)
	}
	return mapCatalogFileError(err)
}

// forgetStores drops the transcript stores cached for files under dir, so a session created
// again under the same name starts from what is on disk and not from the deleted transcript.
func (r *Resolver) forgetStores(dir string) {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.stores {
		if strings.HasPrefix(key, prefix) {
			delete(r.stores, key)
		}
	}
}

// CancelAllSessionChildren asks the sub-agent runs of every profile of a session to stop and
// reports how many were still going. It does not wait.
func (r *Resolver) CancelAllSessionChildren(reference string) int {
	r.mu.Lock()
	managers := make([]interface{ CancelOwnersWithPrefix(string) int }, 0, len(r.subagentManagers))
	for _, manager := range r.subagentManagers {
		managers = append(managers, manager)
	}
	r.mu.Unlock()
	prefix := sessionKey(reference, "")
	cancelled := 0
	for _, manager := range managers {
		cancelled += manager.CancelOwnersWithPrefix(prefix)
	}
	return cancelled
}

// CanonicalSessionReference implements httpapi.SessionReferences. It does not need the
// session to exist, except to choose a root for an unqualified reference with several roots.
func (r *Resolver) CanonicalSessionReference(reference string) (string, error) {
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return "", err
	}
	return location.Reference, nil
}

// CanonicalSessionProfile implements httpapi.SessionProfiles. A named profile is returned as
// it is; an empty one becomes the session's default profile, read without loading the
// session's settings.
func (r *Resolver) CanonicalSessionProfile(reference, profile string) (string, error) {
	if profile != "" {
		return profile, nil
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return "", err
	}
	if !location.UsesLayout() {
		return profile, nil
	}
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil {
		return "", err
	}
	return info.ActiveProfile, nil
}
