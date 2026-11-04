package app

import (
	"context"
	"errors"
	"jad/backend-go/internal/imagestore"
	"net/http"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

func (r *Resolver) UpdateModelDefault(ctx context.Context, reference, requestedProfile, modelDefault, ifMatch string) (httpapi.SessionModelDefaultUpdate, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionModelDefaultUpdate{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapSessionError(err)
	}

	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	info, err := sessionfs.ReadLayoutProfileInfo(location)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	profile := requestedProfile
	if profile == "" {
		profile = info.ActiveProfile
	}
	if profile == "" || len(info.Profiles) == 0 {
		return httpapi.SessionModelDefaultUpdate{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "model config path is not available in the active session path profile"}
	}
	settings, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	modelDefault = strings.TrimSpace(modelDefault)
	modelID, _, err := sessionfs.SplitModelEffort(modelDefault)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	if !containsSessionPath(settings.Models, modelID) {
		return httpapi.SessionModelDefaultUpdate{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "model config path is not available in the active session path profile"}
	}
	selected, err := sessionfs.LoadSessionConfig(location, profile, modelDefault)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	update, err := sessionfs.SetLayoutModelDefaultIfMatch(location, profile, modelDefault, settings.Persistence.Durability, ifMatch)
	if err != nil {
		return httpapi.SessionModelDefaultUpdate{}, mapProfileSettingsError(err)
	}
	models := settings.Models
	if models == nil {
		models = []string{modelID}
	}
	return httpapi.SessionModelDefaultUpdate{
		ActiveProfile: update.ActiveProfile,
		Profiles:      update.Profiles,
		ModelDefault:  update.ModelDefault,
		Models:        append([]string{}, models...),
		ModelOptions:  listingModelOptions(settings),
		Model: httpapi.SessionModelInfo{
			Provider: selected.Model.Provider,
			Model:    selected.Model.Name,
		},
		Changed:     update.Changed,
		ProfileETag: update.ETag,
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
	var conflictErr *sessionfs.ConflictError
	if errors.As(err, &conflictErr) {
		return &httpapi.StatusError{Code: http.StatusConflict, Message: conflictErr.Message}
	}
	var catalogErr *sessionfs.CatalogError
	if errors.As(err, &catalogErr) && catalogErr.Kind == sessionfs.CatalogPreconditionFailed {
		return &httpapi.StatusError{Code: http.StatusPreconditionFailed, Message: catalogErr.Message}
	}
	var clientErr *sessionfs.ClientError
	if errors.As(err, &clientErr) {
		return &httpapi.StatusError{Code: http.StatusBadRequest, Message: clientErr.Message}
	}
	return mapSessionError(err)
}

// ReadSessionImage serves one stored image of a session. The name must be a stored file
// name; the lookup never builds a path from anything else.
func (r *Resolver) ReadSessionImage(ctx context.Context, reference, profile, name string) (httpapi.SessionImage, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return httpapi.SessionImage{}, err
	}
	if _, valid := imagestore.ValidName(name); !valid {
		return httpapi.SessionImage{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "invalid image name"}
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionImage{}, mapSessionError(err)
	}
	settings, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionImage{}, mapSessionError(err)
	}
	store := newImageStore(settings.ImageStore)
	if store == nil {
		return httpapi.SessionImage{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "image store is off"}
	}
	data, contentType, err := store.Open(name)
	if err != nil {
		return httpapi.SessionImage{}, &httpapi.StatusError{Code: http.StatusNotFound, Message: "image not found"}
	}
	return httpapi.SessionImage{ContentType: contentType, Data: data}, nil
}
