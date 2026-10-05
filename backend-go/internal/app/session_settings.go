package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func (r *Resolver) ReadPathProfileInfo(ctx context.Context, reference string) (httpapi.SessionPathProfileInfo, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionPathProfileInfo{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionPathProfileInfo{}, mapSessionError(err)
	}
	info, err := sessionfs.ReadPathProfileInfo(filepathSettingPath(location))
	if err != nil {
		return httpapi.SessionPathProfileInfo{}, mapProfileSettingsError(err)
	}
	return httpapi.SessionPathProfileInfo{ActiveProfile: info.ActiveProfile, PathProfiles: info.PathProfiles}, nil
}

func (r *Resolver) UpdatePathProfile(ctx context.Context, reference, activeProfile string) (httpapi.SessionPathProfileUpdate, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionPathProfileUpdate{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapSessionError(err)
	}

	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	settingPath := filepathSettingPath(location)
	info, err := sessionfs.ReadPathProfileInfo(settingPath)
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	if len(info.PathProfiles) == 0 {
		return httpapi.SessionPathProfileUpdate{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "session has no configured path profiles"}
	}
	current, err := sessionfs.LoadSessionConfig(location, info.ActiveProfile, "")
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	target, err := sessionfs.LoadSessionConfig(location, activeProfile, "")
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	var storagePaths map[string]sessionfs.SessionStoragePaths
	if info.ActiveProfile != "" && info.ActiveProfile != activeProfile {
		storagePaths = map[string]sessionfs.SessionStoragePaths{
			info.ActiveProfile: current.StoragePaths,
			activeProfile:      target.StoragePaths,
		}
	}
	update, err := sessionfs.SetPathProfile(settingPath, activeProfile, current.Persistence.Durability, storagePaths)
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	target, err = sessionfs.LoadSessionConfig(location, activeProfile, "")
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	updatedInfo, err := sessionfs.ReadPathProfileInfo(settingPath)
	if err != nil {
		return httpapi.SessionPathProfileUpdate{}, mapProfileSettingsError(err)
	}
	response := httpapi.SessionPathProfileUpdate{
		ActiveProfile: update.ActiveProfile,
		PathProfiles:  updatedInfo.PathProfiles,
		Changed:       update.Changed,
	}
	if target.ModelDefault != "" {
		modelDefault := target.ModelDefault
		response.ModelDefault = &modelDefault
	}
	if target.Models != nil {
		models := append([]string{}, target.Models...)
		response.Models = &models
	}
	return response, nil
}

func (r *Resolver) UpdateModelDefault(ctx context.Context, reference, requestedProfile, modelDefault string) (httpapi.SessionModelDefaultUpdate, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionModelDefaultUpdate{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapSessionError(err)
	}

	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	settingPath := filepathSettingPath(location)
	info, err := sessionfs.ReadPathProfileInfo(settingPath)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	profile := requestedProfile
	if profile == "" {
		profile = info.ActiveProfile
	}
	if profile == "" || len(info.PathProfiles) == 0 {
		return httpapi.SessionModelDefaultUpdate{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "model config path is not available in the active session path profile"}
	}
	settings, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	if !containsSessionPath(settings.Models, modelDefault) {
		return httpapi.SessionModelDefaultUpdate{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "model config path is not available in the active session path profile"}
	}
	selected, err := sessionfs.LoadSessionConfig(location, profile, modelDefault)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	update, err := sessionfs.SetModelDefault(settingPath, profile, strings.TrimSpace(modelDefault), settings.Persistence.Durability)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	models := settings.Models
	if models == nil {
		models = []string{update.ModelDefault}
	}
	return httpapi.SessionModelDefaultUpdate{
		ActiveProfile: update.ActiveProfile,
		PathProfiles:  update.PathProfiles,
		ModelDefault:  update.ModelDefault,
		Models:        append([]string{}, models...),
		Model: httpapi.SessionModelInfo{
			Provider: selected.Model.Provider,
			Model:    selected.Model.Name,
		},
		Changed: update.Changed,
	}, nil
}

func (r *Resolver) ListSessionFiles(ctx context.Context, reference, profile string) (httpapi.SessionFileListing, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionFileListing{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionFileListing{}, mapSessionError(err)
	}
	settings, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionFileListing{}, mapSessionError(err)
	}
	if !settings.SessionFiles.Enabled {
		return httpapi.SessionFileListing{}, &httpapi.StatusError{Code: http.StatusForbidden, Message: "file listing is disabled"}
	}
	listing := sessionfs.CollectSessionFiles(settings.WorkingDir, settings.SessionFiles, settings.NativeReadPolicy)
	return httpapi.SessionFileListing{Files: listing.Files, Truncated: listing.Truncated}, nil
}

func requireSettingsContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("session settings context is required")
	}
	return ctx.Err()
}

func filepathSettingPath(location sessionfs.Location) string {
	return filepath.Join(location.Directory, "setting.json")
}

func containsSessionPath(paths []string, value string) bool {
	for _, path := range paths {
		if path == value {
			return true
		}
	}
	return false
}

func mapProfileSettingsError(err error) error {
	var statusErr *httpapi.StatusError
	if errors.As(err, &statusErr) {
		return statusErr
	}
	var configErr *sessionfs.ConfigError
	if errors.As(err, &configErr) {
		return &httpapi.StatusError{Code: http.StatusBadRequest, Message: configErr.Message}
	}
	var clientErr *sessionfs.ClientError
	if errors.As(err, &clientErr) {
		return &httpapi.StatusError{Code: http.StatusBadRequest, Message: clientErr.Message}
	}
	return mapSessionError(err)
}
