package app

import (
	"context"
	"errors"
	"net/http"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

var catalogFileKinds = map[string]sessionfs.CatalogKind{
	"profiles": sessionfs.CatalogProfiles,
	"models":   sessionfs.CatalogModels,
	"toolsets": sessionfs.CatalogToolsets,
}

func (r *Resolver) catalogLayout(ctx context.Context, kind string) (sessionfs.Layout, sessionfs.CatalogKind, error) {
	if err := requireSettingsContext(ctx); err != nil {
		return sessionfs.Layout{}, "", err
	}
	known, ok := catalogFileKinds[kind]
	if !ok {
		return sessionfs.Layout{}, "", &httpapi.StatusError{Code: http.StatusNotFound, Message: "unknown catalog"}
	}
	for _, root := range r.roots {
		if root.Layout != nil {
			return *root.Layout, known, nil
		}
	}
	return sessionfs.Layout{}, "", &httpapi.StatusError{Code: http.StatusNotFound, Message: "no catalog layout is configured"}
}

func (r *Resolver) ListCatalogFiles(ctx context.Context, kind string) ([]httpapi.CatalogFileInfo, error) {
	layout, known, err := r.catalogLayout(ctx, kind)
	if err != nil {
		return nil, err
	}
	files, err := sessionfs.ListCatalogFiles(layout, known)
	if err != nil {
		return nil, mapCatalogFileError(err)
	}
	listing := make([]httpapi.CatalogFileInfo, len(files))
	for index, file := range files {
		listing[index] = httpapi.CatalogFileInfo{ID: file.ID, ETag: file.ETag}
	}
	return listing, nil
}

func (r *Resolver) ReadCatalogFile(ctx context.Context, kind, id string) (httpapi.CatalogFile, error) {
	layout, known, err := r.catalogLayout(ctx, kind)
	if err != nil {
		return httpapi.CatalogFile{}, err
	}
	file, err := sessionfs.ReadCatalogFile(layout, known, id)
	if err != nil {
		return httpapi.CatalogFile{}, mapCatalogFileError(err)
	}
	return httpapi.CatalogFile{CatalogFileInfo: httpapi.CatalogFileInfo{ID: file.ID, ETag: file.ETag}, Data: file.Data}, nil
}

func (r *Resolver) WriteCatalogFile(ctx context.Context, kind, id string, data []byte, ifMatch, ifNoneMatch string) (httpapi.CatalogFileInfo, bool, error) {
	layout, known, err := r.catalogLayout(ctx, kind)
	if err != nil {
		return httpapi.CatalogFileInfo{}, false, err
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	info, created, err := sessionfs.WriteCatalogFile(layout, r.roots, known, id, data, sessionfs.WriteCondition{IfMatch: ifMatch, IfNoneMatch: ifNoneMatch}, true)
	if err != nil {
		return httpapi.CatalogFileInfo{}, false, mapCatalogFileError(err)
	}
	return httpapi.CatalogFileInfo{ID: info.ID, ETag: info.ETag}, created, nil
}

func (r *Resolver) DeleteCatalogFile(ctx context.Context, kind, id, ifMatch string) error {
	layout, known, err := r.catalogLayout(ctx, kind)
	if err != nil {
		return err
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	return mapCatalogFileError(sessionfs.DeleteCatalogFile(layout, r.roots, known, id, ifMatch))
}

func mapCatalogFileError(err error) error {
	if err == nil {
		return nil
	}
	var catalogErr *sessionfs.CatalogError
	if errors.As(err, &catalogErr) {
		code := http.StatusBadRequest
		switch catalogErr.Kind {
		case sessionfs.CatalogNotFound:
			code = http.StatusNotFound
		case sessionfs.CatalogPreconditionFailed:
			code = http.StatusPreconditionFailed
		case sessionfs.CatalogPreconditionRequired:
			code = http.StatusPreconditionRequired
		case sessionfs.CatalogInUse:
			code = http.StatusConflict
		}
		return &httpapi.StatusError{Code: code, Message: catalogErr.Message}
	}
	var conflictErr *sessionfs.ConflictError
	if errors.As(err, &conflictErr) {
		return &httpapi.StatusError{Code: http.StatusPreconditionFailed, Message: conflictErr.Message}
	}
	return mapSessionError(err)
}
