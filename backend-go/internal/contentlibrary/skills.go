package contentlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// maxSkillNameLength is the longest skill name the library accepts, in UTF-16
// units like the TypeScript backend. The name pattern is ASCII, so it is also
// the byte length.
const maxSkillNameLength = 64

// LoadedSkill is one skill together with the directory it was loaded from.
// Relative files the skill mentions resolve against Directory.
type LoadedSkill struct {
	Asset     Asset
	Directory string
}

// SkillDirectories returns the skill directories named by the global root's own
// setting.json. A root without a setting.json uses the defaults. Unlike the
// root-library service it addresses the root directly instead of discovering
// the setting file from the working directory, and the file is read on every
// call so an edit applies without a restart.
func SkillDirectories(globalJadRoot string) ([]string, error) {
	root, err := filepath.Abs(globalJadRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve global root: %w", err)
	}
	settingPath := filepath.Join(root, "setting.json")
	label := root
	var setting map[string]json.RawMessage
	info, err := os.Stat(settingPath)
	switch {
	case err == nil && info.Mode().IsRegular():
		if setting, err = readLibrarySetting(settingPath); err != nil {
			return nil, err
		}
		label = settingPath
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	var configured map[string]json.RawMessage
	if raw, exists := setting["libraries"]; exists {
		if err := json.Unmarshal(raw, &configured); err != nil || configured == nil {
			return nil, configError("%s: libraries must be an object", label)
		}
	}
	var defaults libraryDefaults
	if err := json.Unmarshal(embeddedDefaults, &defaults); err != nil {
		return nil, fmt.Errorf("decode embedded root-library defaults: %w", err)
	}
	return resolveLibraryDirectoryList(configured, "skill_dirs", defaults.Skills, label, root)
}

// ListSkills returns the skills visible across directories, in directory order.
// When two directories define the same name the earlier one wins, so a project
// can override a shared skill. The result is sorted by name.
func ListSkills(ctx context.Context, directories []string, maxBytes int64) ([]Asset, error) {
	if ctx == nil {
		return nil, errors.New("skill request context is required")
	}
	byName := make(map[string]Asset)
	for _, root := range directories {
		files, err := filesForKind(ctx, Skill, []string{root})
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			asset, err := readAsset(Skill, file.root, file.path, maxBytes)
			if err != nil {
				return nil, err
			}
			if asset == nil {
				continue
			}
			if _, exists := byName[asset.Name]; !exists {
				byName[asset.Name] = *asset
			}
		}
	}
	skills := make([]Asset, 0, len(byName))
	for _, asset := range byName {
		skills = append(skills, asset)
	}
	sort.Slice(skills, func(i, j int) bool { return localeCompare(skills[i].Name, skills[j].Name) < 0 })
	return skills, nil
}

// ReadSkill returns one skill by name. A valid skill's folder name equals its
// name, so the file is addressed directly instead of scanning every skill; the
// name pattern rules out path separators and "..".
func ReadSkill(ctx context.Context, directories []string, name string, maxBytes int64) (LoadedSkill, bool, error) {
	if ctx == nil {
		return LoadedSkill{}, false, errors.New("skill request context is required")
	}
	if len(name) > maxSkillNameLength || !skillNamePattern.MatchString(name) {
		return LoadedSkill{}, false, nil
	}
	for _, root := range directories {
		if err := ctx.Err(); err != nil {
			return LoadedSkill{}, false, err
		}
		directory := filepath.Join(root, name)
		asset, err := readAsset(Skill, root, filepath.Join(directory, "SKILL.md"), maxBytes)
		if err != nil {
			return LoadedSkill{}, false, err
		}
		if asset != nil {
			return LoadedSkill{Asset: *asset, Directory: directory}, true, nil
		}
	}
	return LoadedSkill{}, false, nil
}
