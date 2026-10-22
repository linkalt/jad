package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
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
	location, locateErr := sessionfs.Resolve(r.roots, reference)
	stateDir, err := sessionfs.DeleteSessionFile(r.roots, reference, ifMatch, deleteState)
	if stateDir != "" {
		r.forgetStores(stateDir)
	}
	if locateErr == nil {
		r.listSettingsCache.drop(location.File)
	}
	if locateErr == nil && (err == nil || sessionFileGone(location.File)) {
		r.releaseSessionChildren(location.File)
	}
	return mapCatalogFileError(err)
}

// sessionFileGone reports whether a delete that returned an error had already removed the
// session file; the session is unreachable then, so its children must still be released.
func sessionFileGone(file string) bool {
	_, err := os.Stat(file)
	return errors.Is(err, fs.ErrNotExist)
}

// releaseSessionChildren stops the MCP server processes and the managed processes that
// belong to the deleted session file, and drops the task, memory and fetch stores no other
// session uses. A store held by a run that outlives the delete keeps working; a later
// access builds a new one over the same files. Without it they would run until the server stops,
// with no session left to reach them. A session created again under the same name gets
// new managers.
func (r *Resolver) releaseSessionChildren(settingPath string) {
	settingPath = filepath.Clean(settingPath)
	mcpPrefix := settingPath + "\x00"
	r.mu.Lock()
	var mcpManagers []*mcpclient.Manager
	for key, manager := range r.mcpManagers {
		if strings.HasPrefix(key, mcpPrefix) {
			mcpManagers = append(mcpManagers, manager)
			delete(r.mcpManagers, key)
		}
	}
	var processManagers []*native.ManagedProcessManager
	for key, sessions := range r.processSessions {
		if _, used := sessions[settingPath]; !used {
			continue
		}
		delete(sessions, settingPath)
		if len(sessions) == 0 {
			processManagers = append(processManagers, r.processManagers[key])
			delete(r.processManagers, key)
			delete(r.processSessions, key)
		}
	}
	var fetchStores []*native.FetchStore
	for key, sessions := range r.storeSessions {
		if _, used := sessions[settingPath]; !used {
			continue
		}
		delete(sessions, settingPath)
		if len(sessions) > 0 {
			continue
		}
		delete(r.storeSessions, key)
		kind, storageKey, _ := strings.Cut(key, "\x00")
		switch kind {
		case "task":
			delete(r.taskStores, storageKey)
		case "memory":
			delete(r.memoryStores, storageKey)
		case "fetch":
			fetchStores = append(fetchStores, r.fetchStores[storageKey])
			delete(r.fetchStores, storageKey)
		}
	}
	r.mu.Unlock()
	for _, store := range fetchStores {
		store.Close()
	}
	for _, manager := range mcpManagers {
		_ = manager.Close()
	}
	for _, manager := range processManagers {
		_ = manager.Close()
	}
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

// CancelAllSessionChildren stops the handles of every profile of a session (processes and
// sub-agent runs, also on a storage other sessions share) and removes their records from
// the async manager, and reports how many were still going. It does not wait. The other
// sessions' handles are left alone.
func (r *Resolver) CancelAllSessionChildren(reference string) int {
	prefix := sessionKey(reference, "")
	cancelled := 0
	for _, manager := range r.asyncManagers() {
		cancelled += manager.DeleteOwnersWithPrefix(prefix, reasonSessionDelete)
	}
	subagents := 0
	for _, manager := range r.subagentManagerList() {
		subagents += manager.CancelUnmanagedOwnersWithPrefix(prefix)
	}
	return cancelled + subagents
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
