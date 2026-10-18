package sessionfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/catalog"
)

// ReadLayoutProfileInfo returns the profiles a catalog-layout session offers and its
// active profile, both as <category>/<name> IDs.
func ReadLayoutProfileInfo(location Location) (ProfileInfo, error) {
	layout := *location.Root.Layout
	session, err := readLayoutSession(location)
	if err != nil {
		return ProfileInfo{}, err
	}
	profiles, err := scopedProfiles(layout, session, location.SettingFile())
	if err != nil {
		return ProfileInfo{}, err
	}
	return ProfileInfo{ActiveProfile: session.defaultProfile(profiles), Profiles: profiles.IDs()}, nil
}

func readLayoutSession(location Location) (layoutSessionFile, error) {
	var session layoutSessionFile
	if err := readLayoutFile(location.SettingFile(), &session); err != nil {
		return layoutSessionFile{}, err
	}
	if err := session.refuseModelDefaults(location.SettingFile()); err != nil {
		return layoutSessionFile{}, err
	}
	return session, nil
}

func scopedProfiles(layout Layout, session layoutSessionFile, sessionPath string) (*catalog.Catalog, error) {
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%s: %v", sessionPath, err)
	}
	if profiles, err = profiles.Narrow(session.Scope.Profiles); err != nil {
		return nil, configError("%s: scope.profiles: %v", sessionPath, err)
	}
	return profiles, nil
}

// ConflictError reports that a file changed between the read and the write of an update,
// so the update was not made. The client may read again and retry.
type ConflictError struct {
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// SetLayoutModelDefault makes model the model_default of the profile's own file, so the
// choice holds for every session that uses the profile. The file is rewritten with its key
// order and indentation kept. When the file changes between the read and the write, nothing
// is written and a ConflictError is returned.
func SetLayoutModelDefault(location Location, profile, model string, durability string) (ModelDefaultUpdate, error) {
	layout := *location.Root.Layout
	sessionPath := location.SettingFile()
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelDefaultUpdate{}, &ClientError{Message: "model_default must be a non-empty string"}
	}
	id, effort, err := SplitModelEffort(model)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	session, err := readLayoutSession(location)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	profiles, err := scopedProfiles(layout, session, sessionPath)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	entry, found := profiles.Get(profile)
	if !found {
		return ModelDefaultUpdate{}, &ClientError{Message: fmt.Sprintf("profile '%s' is not available to this session", profile)}
	}
	before, err := os.Stat(entry.Path)
	if err != nil {
		return ModelDefaultUpdate{}, configError("%s: could not be read: %v", entry.Path, err)
	}
	contents, err := os.ReadFile(entry.Path)
	if err != nil {
		return ModelDefaultUpdate{}, configError("%s: could not be read: %v", entry.Path, err)
	}
	var file layoutProfileFile
	if err := readLayoutFile(entry.Path, &file); err != nil {
		return ModelDefaultUpdate{}, err
	}
	models, err := catalog.Scan("model", layout.Models)
	if err != nil {
		return ModelDefaultUpdate{}, configError("%s: %v", entry.Path, err)
	}
	if models, err = models.Narrow(file.Models); err != nil {
		return ModelDefaultUpdate{}, configError("%s: models: %v", entry.Path, err)
	}
	modelEntry, offered := models.Get(id)
	if !offered {
		return ModelDefaultUpdate{}, &ClientError{Message: fmt.Sprintf("model '%s' is not offered by profile '%s'", id, profile)}
	}
	if err := checkModelEffort(modelEntry.Path, effort); err != nil {
		return ModelDefaultUpdate{}, err
	}
	update := ModelDefaultUpdate{ActiveProfile: profile, Profiles: profiles.IDs(), ModelDefault: model, Changed: file.ModelDefault != model}
	if !update.Changed {
		return update, nil
	}
	rewritten, err := withModelDefault(contents, model)
	if err != nil {
		return ModelDefaultUpdate{}, configError("%s: %v", entry.Path, err)
	}
	if err := writeFileIfUnchanged(entry.Path, rewritten, before, durability == "strict"); err != nil {
		return ModelDefaultUpdate{}, err
	}
	return update, nil
}

// withModelDefault returns the profile file with model_default set, keeping the order of
// the other keys and the file's indentation. A new key goes after models, else last.
func withModelDefault(contents []byte, model string) ([]byte, error) {
	keys, err := orderedObjectKeys(contents)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		return nil, err
	}
	if fields["model_default"], err = json.Marshal(model); err != nil {
		return nil, err
	}
	if !containsString(keys, "model_default") {
		at := len(keys)
		for index, key := range keys {
			if key == "models" {
				at = index + 1
			}
		}
		keys = append(keys[:at], append([]string{"model_default"}, keys[at:]...)...)
	}
	compact, err := marshalOrderedObject(fields, keys)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", indentOf(contents)); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// indentOf is the indentation of the first indented line, two spaces when there is none.
func indentOf(contents []byte) string {
	for _, line := range strings.Split(string(contents), "\n")[1:] {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != "" && len(trimmed) < len(line) {
			return line[:len(line)-len(trimmed)]
		}
	}
	return "  "
}

// writeFileIfUnchanged replaces path with data through a temporary file and a rename, keeping
// the file's mode, but only when its size and modification time still equal before.
func writeFileIfUnchanged(path string, data []byte, before os.FileInfo, syncWrite bool) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(before.Mode().Perm()); err != nil {
		_ = file.Close()
		return err
	}
	if syncWrite {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	now, err := os.Stat(path)
	if err != nil || now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		return &ConflictError{Message: fmt.Sprintf("%s changed while it was being updated; read it again and retry", path)}
	}
	return os.Rename(temporaryPath, path)
}
