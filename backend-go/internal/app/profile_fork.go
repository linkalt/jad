package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

// forkIntoProfile forks a session profile into a new profile of the same session: the
// source profile's file is copied under request.Profile in the same category (with the
// title as its name), the session's scope gets the new profile when it does not already
// select it, and the transcript up to the fork point is copied to the new profile's state.
// A failure removes the new profile file and puts the session file back.
func (r *Resolver) forkIntoProfile(ctx context.Context, source sessionfs.Location, profile string, request httpapi.ForkRequest) (httpapi.ForkResult, error) {
	config, err := sessionfs.LoadSessionSettings(source, profile, "")
	if err != nil {
		return httpapi.ForkResult{}, mapSessionError(err)
	}
	category, _, found := strings.Cut(config.Profile, "/")
	if !found || config.ProfilePath == "" {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "only a catalog profile (<category>/<name>) can be forked into a new profile"}
	}
	newProfile := category + "/" + request.Profile
	if newProfile == config.Profile {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusBadRequest, Message: "a fork needs a new profile name"}
	}

	key := sessionKey(source.Reference, config.Profile)
	if !r.gate.Begin(key) {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusConflict, Message: "session profile is already changing"}
	}
	defer r.gate.End(key)
	unlock := r.gate.Lock(source.Reference)
	defer unlock()
	if active := r.runActive.Load(); active != nil && (*active)(source.Reference, config.Profile) {
		return httpapi.ForkResult{}, &httpapi.StatusError{Code: http.StatusConflict, Message: "cannot fork a profile while a run is active"}
	}

	point, err := r.forkPoint(ctx, config, request)
	if err != nil {
		return httpapi.ForkResult{}, err
	}
	sourceFile, err := r.ReadCatalogFile(ctx, "profiles", config.Profile)
	if err != nil {
		return httpapi.ForkResult{}, err
	}
	data := sourceFile.Data
	if request.Title != "" {
		if data, err = setTopLevelJSONKey(data, "name", request.Title); err != nil {
			return httpapi.ForkResult{}, fmt.Errorf("copy the profile file: %w", err)
		}
	}
	if _, _, err := r.WriteCatalogFile(ctx, "profiles", newProfile, data, "", "*"); err != nil {
		return httpapi.ForkResult{}, err
	}
	undo := []func(){func() { _ = r.DeleteCatalogFile(context.WithoutCancel(ctx), "profiles", newProfile, "*") }}
	fail := func(err error) (httpapi.ForkResult, error) {
		for index := len(undo) - 1; index >= 0; index-- {
			undo[index]()
		}
		return httpapi.ForkResult{}, err
	}
	restoreScope, err := r.addProfileToScope(ctx, source, newProfile)
	if err != nil {
		return fail(err)
	}
	if restoreScope != nil {
		undo = append(undo, restoreScope)
	}
	result, err := r.writeFork(ctx, source, newProfile, config, point)
	if err != nil {
		return fail(err)
	}
	result.Profile = newProfile
	return result, nil
}

// addProfileToScope makes the session select profile: when its scope.profiles does not
// already hold it, the profile's selector is appended to that list. It returns the function
// that puts the session file back, or nil when nothing was written.
func (r *Resolver) addProfileToScope(ctx context.Context, location sessionfs.Location, profile string) (func(), error) {
	if config, err := sessionfs.LoadSessionSettings(location, "", ""); err == nil && slices.Contains(config.Profiles, profile) {
		return nil, nil
	}
	file, err := sessionfs.ReadSessionFile(r.roots, location.Reference)
	if err != nil {
		return nil, mapCatalogFileError(err)
	}
	var session struct {
		Scope struct {
			Profiles json.RawMessage `json:"profiles"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(file.Data, &session); err != nil {
		return nil, fmt.Errorf("read the session file: %w", err)
	}
	var selectors []string
	if err := json.Unmarshal(session.Scope.Profiles, &selectors); err != nil {
		return nil, &httpapi.StatusError{Code: http.StatusConflict, Message: "the session's scope.profiles does not select the new profile and is not a list it can be added to"}
	}
	selectors = append(selectors, profile)
	scope, err := setTopLevelJSONKey(rawObjectOr(file.Data, "scope"), "profiles", selectors)
	if err != nil {
		return nil, err
	}
	updated, err := setTopLevelJSONKey(file.Data, "scope", json.RawMessage(scope))
	if err != nil {
		return nil, err
	}
	written, _, err := r.WriteSessionConfig(ctx, location.Reference, updated, file.ETag, "")
	if err != nil {
		return nil, err
	}
	original := file.Data
	return func() {
		_, _, _ = r.WriteSessionConfig(context.WithoutCancel(ctx), location.Reference, original, written.ETag, "")
	}, nil
}

// rawObjectOr returns the raw value of key in the JSON object data, or {} when absent.
func rawObjectOr(data []byte, key string) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields[key] == nil {
		return []byte("{}")
	}
	return fields[key]
}

// setTopLevelJSONKey sets key in the JSON object data to value, keeping the order of the
// other keys (a new key goes last). The result is indented like a catalog file.
func setTopLevelJSONKey(data []byte, key string, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	replaced := false
	first := true
	write := func(name string, raw []byte) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		quoted, _ := json.Marshal(name)
		out.Write(quoted)
		out.WriteByte(':')
		out.Write(raw)
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, _ := token.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if name == key {
			raw, replaced = encoded, true
		}
		write(name, raw)
	}
	if !replaced {
		write(key, encoded)
	}
	out.WriteByte('}')
	var indented bytes.Buffer
	if err := json.Indent(&indented, out.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}
