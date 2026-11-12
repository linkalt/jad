package sessionfs

import (
	"path/filepath"
)

// writeTarget is a folder or file an in-process writer of a loaded configuration writes,
// named by the setting it comes from.
type writeTarget struct {
	setting, path string
}

// writeTargets are the folders and files the configuration's in-process writers write, as
// the loader resolved them (placeholders, the relative base and Clean applied): transcripts,
// caches and staging, tool storage, the storage of the task, records, code_context and
// subagent tools, the spill folders and files of exec and tool_smith, the image store, the
// subagent worktree folder and the function libraries of exec and tool_smith. A relative
// functions_dir is taken from the cwd, as the tools resolve it for a top-level run.
func (c SessionConfig) writeTargets() []writeTarget {
	storage := c.StoragePaths
	targets := []writeTarget{
		{"messages", c.MessagesPath}, {"messages", storage.Messages}, {"messages_backup", c.MessagesBackupPath}, {"messages_backup", storage.MessagesBackup},
		{"usage_cache", c.UsageCachePath}, {"usage_cache", storage.UsageCache}, {"compaction_cache", storage.CompactionCache}, {"cursor_cache", storage.CursorCache},
		{"run_error_cache", storage.RunErrorCache}, {"statecontext", storage.StateContext}, {"staging", c.StagingDir}, {"staging", storage.Staging},
		{"http_debug", storage.HTTPDebug}, {"tool_storage_dir", storage.ToolStorage}, {"image_store.dir", c.ImageStore.Dir},
	}
	if c.NativeTask != nil {
		targets = append(targets, writeTarget{"tools.native.task.storage_dir", c.NativeTask.StorageDir})
	}
	if c.NativeRecords != nil {
		targets = append(targets, writeTarget{"tools.native.records.storage_dir", c.NativeRecords.StorageDir})
	}
	if c.NativeCodeContext != nil {
		targets = append(targets, writeTarget{"tools.native.code_context.storage_dir", c.NativeCodeContext.StorageDir})
	}
	if c.NativeSubagent != nil {
		targets = append(targets, writeTarget{"tools.native.subagent.storage_dir", c.NativeSubagent.StorageDir},
			writeTarget{"tools.native.subagent.worktree.dir", c.NativeSubagent.Worktree.Dir})
	}
	if c.NativeExec != nil {
		targets = c.appendChildTargets(targets, "tools.native.exec", c.NativeExec.Child, c.NativeExec.FunctionsDir)
	}
	if c.NativeToolSmith != nil {
		targets = c.appendChildTargets(targets, "tools.native.tool_smith", c.NativeToolSmith.Child, c.NativeToolSmith.FunctionsDir)
	}
	return targets
}

// appendChildTargets adds a child tool's spill folder, the folders its spill files are made
// in (a template filled with this session's values) and its function library.
func (c SessionConfig) appendChildTargets(targets []writeTarget, section string, child NativeBashSettings, functionsDir string) []writeTarget {
	targets = append(targets, writeTarget{section + ".spill.storage_dir", child.Spill.StorageDir})
	for _, file := range []struct{ name, template string }{{"stdout_full", child.Spill.StdoutFull}, {"stderr_full", child.Spill.StderrFull}} {
		if file.template != "" {
			targets = append(targets, writeTarget{section + ".spill." + file.name, filepath.Dir(resolveBashSpillTemplate(file.template, c.WorkingDir, c.StateDir, "call", "stdout"))})
		}
	}
	if functionsDir != "" {
		targets = append(targets, writeTarget{section + ".functions_dir", functionsDir})
	}
	return targets
}

// checkWriteTargets refuses a loaded configuration (and each of its agent toolsets) whose
// in-process write target lands in a profile-own folder of any known workspace (3b.1): those
// folders change only through the profile tool, under its lock and with its history. Each
// target is judged where the loader resolved it, after the links of the part that exists, so
// a relative path, .. or a link is judged by where it writes. A self-edit meets the same
// refusal through its trial load; a profile file written by hand fails its load. Workspace
// layout only: v1 has no profile-own folder.
func (l Layout) checkWriteTargets(where string, config SessionConfig) error {
	if len(l.ProfileOwnRoots) == 0 {
		return nil
	}
	for _, target := range config.writeTargets() {
		if target.path == "" {
			continue
		}
		path := target.path
		if !filepath.IsAbs(path) {
			if !filepath.IsAbs(config.WorkingDir) {
				continue
			}
			path = filepath.Join(config.WorkingDir, path)
		}
		resolved := resolveExisting(filepath.Clean(path))
		for _, root := range l.ProfileOwnRoots {
			if within(resolved, root) {
				return configError("%s: %s writes %s, in a profile-own folder (%s): the files there change only through the profile tool's edit", where, target.setting, resolved, root)
			}
		}
	}
	for _, derived := range config.AgentToolsets {
		if err := l.checkWriteTargets(where, derived); err != nil {
			return err
		}
	}
	return nil
}
