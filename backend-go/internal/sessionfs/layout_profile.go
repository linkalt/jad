package sessionfs

import (
	"strings"

	"jad/backend-go/internal/catalog"
)

// readProfile reads one profile file and checks what can be checked without composing the
// setting: schema_version, settings keys, the models it lists and its model_default. It
// returns the profile and the model catalog narrowed to the profile's models.
func readProfile(path string, allModels *catalog.Catalog) (layoutProfileFile, *catalog.Catalog, error) {
	var profile layoutProfileFile
	if err := readLayoutFile(path, &profile); err != nil {
		return layoutProfileFile{}, nil, err
	}
	return checkProfile(path, profile, allModels)
}

func checkProfile(path string, profile layoutProfileFile, allModels *catalog.Catalog) (layoutProfileFile, *catalog.Catalog, error) {
	if profile.SchemaVersion != 0 && profile.SchemaVersion != 1 {
		return layoutProfileFile{}, nil, configError("%s: unsupported schema_version %d", path, profile.SchemaVersion)
	}
	if err := checkLayoutSettings(profile.Settings, path+": settings"); err != nil {
		return layoutProfileFile{}, nil, err
	}
	models, err := allModels.Narrow(profile.Models)
	if err != nil {
		return layoutProfileFile{}, nil, configError("%s: models: %v", path, err)
	}
	if profile.ModelDefault != "" {
		if _, err := models.Lookup(profile.ModelDefault); err != nil {
			return layoutProfileFile{}, nil, configError("%s: model_default: %v", path, err)
		}
	}
	return profile, models, nil
}

// checkUnselectedProfile applies to a profile the session does not run the checks that the
// normalizers apply to the selected one (instructions, prompt document entries, tools), so
// a mistake in any profile of the session's scope is reported when the session loads and
// not when someone switches to it. It does not read the prompt documents or the toolset.
func checkUnselectedProfile(path string, profile layoutProfileFile) error {
	if profile.Instructions != nil {
		if _, ok := nonEmptyJSONString(profile.Instructions); !ok {
			return configError("%s: instructions must be a non-empty string", path)
		}
	}
	for index, document := range profile.PromptDocuments {
		if strings.TrimSpace(document) == "" {
			return configError("%s: prompt_documents[%d] must be a non-empty string", path, index)
		}
	}
	if len(profile.Tools) > 0 {
		if _, err := normalizeProfileTools(profile.Tools, path+": profile"); err != nil {
			return err
		}
	}
	return nil
}
