package sessionfs

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// settingsInputs are the files a settings load read that the catalog scans do not see.
type settingsInputs struct {
	files       []string
	listings    []string
	directories []string
}

func (in *settingsInputs) addFile(path string) {
	if path != "" {
		in.files = append(in.files, path)
	}
}

// collect adds what config was built from: documents, env files, the agents_dir folders
// and the working directory.
func (in *settingsInputs) collect(config SessionConfig) {
	in.directories = append(in.directories, config.WorkingDir)
	for _, document := range config.PromptDocuments {
		in.addFile(document)
	}
	for _, trigger := range config.StateContext.Triggers {
		for _, document := range trigger.PromptDocuments {
			in.addFile(document)
		}
	}
	in.addFile(config.Hooks.EnvFile)
	if config.NativeExec != nil {
		in.addFile(config.NativeExec.Child.EnvFile)
	}
	if config.NativeToolSmith != nil {
		in.addFile(config.NativeToolSmith.Child.EnvFile)
	}
	if subagent := config.NativeSubagent; subagent != nil {
		in.listings = append(in.listings, subagent.ConfiguredAgentDirs...)
		for _, directory := range subagent.AgentDirs {
			in.collectAgentFolders(directory)
		}
		for _, agent := range subagent.Agents {
			for _, document := range agent.PromptDocuments {
				in.addFile(document)
			}
		}
	}
	for _, derived := range config.AgentToolsets {
		in.collect(derived)
	}
}

// collectAgentFolders lists every agent folder of an agents_dir, also one whose agent is
// denied or replaced: its files are read and checked all the same.
func (in *settingsInputs) collectAgentFolders(directory string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		folder := filepath.Join(directory, entry.Name())
		in.listings = append(in.listings, folder)
		in.addFile(filepath.Join(folder, subagentDirSettingsFile))
		in.addFile(filepath.Join(folder, subagentDirPromptFile))
	}
}

func inputStamps(config SessionConfig, taken time.Time) []fileStamp {
	var in settingsInputs
	in.collect(config)
	stamps := make([]fileStamp, 0, len(in.files)+len(in.listings)+len(in.directories))
	for _, path := range in.files {
		stamps = append(stamps, statStampAt(path, taken))
	}
	for _, path := range in.listings {
		stamps = append(stamps, statStampAt(path, taken))
	}
	for _, path := range in.directories {
		stamp := statStampAt(path, taken)
		stamp.presence = true
		stamps = append(stamps, stamp)
	}
	return stamps
}
