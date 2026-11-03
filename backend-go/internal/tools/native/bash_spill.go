package native

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func bashGeneratedSpillFilename(policy sessionfs.NativeBashSpillSettings) string {
	prefix := policy.FilePrefix
	if prefix == "" {
		prefix = "bash"
	}
	return prefix + "-{call_id}.{stream}.log"
}

var bashSpillPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

func resolveNativeBashSpillTemplate(template, workspaceRoot, sessionDir, callID, stream string) string {
	values := map[string]string{
		"cwd":          workspaceRoot,
		"session_dir":  sessionDir,
		"session_name": filepath.Base(sessionDir),
		"call_id":      callID,
		"stream":       stream,
	}
	return bashSpillPlaceholderPattern.ReplaceAllStringFunc(template, func(placeholder string) string {
		name := strings.Trim(placeholder, "{}")
		if value, exists := values[name]; exists {
			return value
		}
		return placeholder
	})
}

type bashSpillPlan struct {
	writers map[bashStream]bashSpillSink
}

type bashSpillWriter struct {
	path     string
	capBytes int64
	file     *os.File
	written  int64
	capped   bool
	failure  string
	closed   bool
}

type bashKeptSpill struct {
	path         string
	display      string
	writtenBytes int64
	capped       bool
}

type bashSpillDecision struct {
	stdout *bashKeptSpill
	stderr *bashKeptSpill
	err    string
	hint   string
}

func planBashSpill(
	policy sessionfs.NativeBashSpillSettings,
	workspaceRoot, sessionDir, callID string,
) *bashSpillPlan {
	if !policy.Enabled {
		return nil
	}
	plan := &bashSpillPlan{writers: make(map[bashStream]bashSpillSink, 2)}
	legacy := policy.StdoutFull != "" || policy.StderrFull != ""
	storageDir := policy.StorageDir
	if storageDir == "" {
		storageDir = filepath.Join(workspaceRoot, ".bash-output")
	}
	if !filepath.IsAbs(storageDir) {
		storageDir = filepath.Join(workspaceRoot, storageDir)
	}
	for _, stream := range []bashStream{bashStdout, bashStderr} {
		var path, pruneTemplate string
		if legacy {
			pruneTemplate = policy.StdoutFull
			if stream == bashStderr {
				pruneTemplate = policy.StderrFull
			}
			if pruneTemplate == "" {
				pruneTemplate = "{cwd}/.bash-output/bash-{call_id}.{stream}.log"
			}
			path = resolveNativeBashSpillTemplate(pruneTemplate, workspaceRoot, sessionDir, callID, string(stream))
			if !filepath.IsAbs(path) {
				path = filepath.Join(workspaceRoot, path)
			}
		} else {
			pruneTemplate = bashGeneratedSpillFilename(policy)
			path = filepath.Join(storageDir, strings.NewReplacer(
				"{call_id}", encodeBashSpillCallID(callID),
				"{stream}", string(stream),
			).Replace(pruneTemplate))
		}
		template := pruneTemplate
		pruneBashSpillFilesThrottled(filepath.Dir(path), template+"\x00"+workspaceRoot+"\x00"+sessionDir, func() *regexp.Regexp {
			return bashSpillPruneRegexp(template, workspaceRoot, sessionDir)
		}, policy.RetentionDays, time.Duration(policy.PruneIntervalSeconds)*time.Second)
		plan.writers[stream] = &bashSpillWriter{path: path, capBytes: policy.CapBytes}
	}
	return plan
}

func (writer *bashSpillWriter) write(chunk []byte) {
	if writer == nil || writer.failure != "" || writer.capped || writer.closed || len(chunk) == 0 {
		return
	}
	room := writer.capBytes - writer.written
	if room <= 0 {
		writer.capped = true
		return
	}
	toWrite := chunk
	if int64(len(toWrite)) > room {
		toWrite = toWrite[:int(room)]
		writer.capped = true
	}
	if writer.file == nil {
		if err := os.MkdirAll(filepath.Dir(writer.path), 0o755); err != nil {
			writer.failure = err.Error()
			return
		}
		file, err := os.OpenFile(writer.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			writer.failure = err.Error()
			return
		}
		writer.file = file
	}
	written, err := writer.file.Write(toWrite)
	writer.written += int64(written)
	if err != nil {
		writer.failure = err.Error()
		return
	}
	if written != len(toWrite) {
		writer.failure = "short write"
	}
}

func (writer *bashSpillWriter) close() bashSpillOutcome {
	if writer == nil {
		return bashSpillOutcome{}
	}
	writer.closed = true
	if writer.file != nil {
		if err := writer.file.Close(); err != nil && writer.failure == "" {
			writer.failure = err.Error()
		}
	}
	return bashSpillOutcome{
		path:         writer.path,
		writtenBytes: writer.written,
		capped:       writer.capped,
		err:          writer.failure,
	}
}

func applyBashSpillPolicy(
	policy sessionfs.NativeBashSpillSettings,
	plan *bashSpillPlan,
	result bashChildResult,
	workspaceRoot string,
) bashSpillDecision {
	if plan == nil {
		return bashSpillDecision{}
	}
	decision := bashSpillDecision{}
	var errors []string
	for _, stream := range []bashStream{bashStdout, bashStderr} {
		outcome, exists := result.spill[stream]
		if !exists {
			continue
		}
		if outcome.err != "" {
			errors = append(errors, string(stream)+": "+outcome.err)
			continue
		}
		if outcome.writtenBytes == 0 {
			continue
		}
		streamTruncated := result.output.stdout.truncated
		if stream == bashStderr {
			streamTruncated = result.output.stderr.truncated
		}
		if policy.Keep != "always" && !streamTruncated {
			_ = os.Remove(outcome.path)
			continue
		}
		kept := &bashKeptSpill{
			path:         outcome.path,
			display:      displayPath(workspaceRoot, outcome.path),
			writtenBytes: outcome.writtenBytes,
			capped:       outcome.capped,
		}
		if stream == bashStdout {
			decision.stdout = kept
		} else {
			decision.stderr = kept
		}
	}
	if len(errors) > 0 {
		decision.err = strings.Join(errors, "; ")
	}
	files := make([]string, 0, 2)
	if decision.stdout != nil {
		files = append(files, decision.stdout.display)
	}
	if decision.stderr != nil {
		files = append(files, decision.stderr.display)
	}
	if len(files) > 0 {
		decision.hint = "the complete output is in " + strings.Join(files, " and ") + "; read or grep it instead of re-running the command"
	}
	return decision
}

func encodeBashSpillCallID(callID string) string {
	const maxEncodedLength = 180
	encoded := make([]byte, 0, len(callID))
	const unescaped = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.!~*'()"
	for _, value := range []byte(callID) {
		if strings.IndexByte(unescaped, value) >= 0 {
			encoded = append(encoded, value)
		} else {
			encoded = append(encoded, '%', "0123456789ABCDEF"[value>>4], "0123456789ABCDEF"[value&0x0f])
		}
	}
	if len(encoded) == 0 {
		return "empty"
	}
	if len(encoded) <= maxEncodedLength {
		return string(encoded)
	}
	digest := sha256.Sum256([]byte(callID))
	return string(encoded[:120]) + "-" + hex.EncodeToString(digest[:])[:16]
}

func bashSpillPruneRegexp(template, workspaceRoot, sessionDir string) *regexp.Regexp {
	located := strings.NewReplacer(
		"{cwd}", workspaceRoot,
		"{session_dir}", sessionDir,
		"{session_name}", filepath.Base(sessionDir),
	).Replace(template)
	name := filepath.Base(located)
	escaped := regexp.QuoteMeta(name)
	escaped = strings.ReplaceAll(escaped, regexp.QuoteMeta("{call_id}"), `[^/]+`)
	escaped = strings.ReplaceAll(escaped, regexp.QuoteMeta("{stream}"), `(?:stdout|stderr)`)
	pattern, err := regexp.Compile("^" + escaped + "$")
	if err == nil {
		return pattern
	}
	fallback, _ := regexp.Compile("^" + regexp.QuoteMeta(name) + "$")
	return fallback
}

var bashSpillPrunes = struct {
	sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

// pruneBashSpillFilesThrottled prunes directory at most once per interval for one pattern.
// patternKey names the pattern (the inputs it is built from) and pattern builds it: it is
// only called when a prune is due, so a bash call that prunes nothing compiles nothing.
func pruneBashSpillFilesThrottled(directory, patternKey string, pattern func() *regexp.Regexp, retentionDays int64, interval time.Duration) {
	if retentionDays <= 0 || pattern == nil {
		return
	}
	if interval > 0 {
		key := directory + "\x00" + patternKey
		bashSpillPrunes.Lock()
		now := time.Now()
		// A key older than the interval is due on its next call anyway, so dropping it
		// changes nothing; without this, a template with {call_id} grows the map per call.
		for other, when := range bashSpillPrunes.last {
			if now.Sub(when) >= interval {
				delete(bashSpillPrunes.last, other)
			}
		}
		last, scanned := bashSpillPrunes.last[key]
		due := !scanned || now.Sub(last) >= interval
		if due {
			bashSpillPrunes.last[key] = now
		}
		bashSpillPrunes.Unlock()
		if !due {
			return
		}
	}
	pruneBashSpillFiles(directory, pattern(), retentionDays)
}

func pruneBashSpillFiles(directory string, pattern *regexp.Regexp, retentionDays int64) {
	if retentionDays <= 0 || pattern == nil {
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	for _, entry := range entries {
		if !pattern.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}
