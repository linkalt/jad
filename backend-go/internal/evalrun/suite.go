// Package evalrun runs an eval suite: for each case it prepares a fresh workspace, makes a
// session whose working directory is that workspace, runs one headless turn, then runs the
// case's checks in the workspace and records whether they passed. The suite file format is
// sample/schemes/eval-suite.schema.json.
package evalrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Suite defaults. Each is a key of the suite file; these are the values a missing key
// takes, mirrored by the "default" of the key in eval-suite.schema.json.
const (
	DefaultRepeat              = 1
	DefaultParallel            = 1
	DefaultRunTimeoutMS        = 30 * 60 * 1000
	DefaultCheckTimeoutMS      = 10 * 60 * 1000
	DefaultCheckOutputMaxBytes = 16 * 1024
	DefaultSessionCategory     = "eval"
	DefaultKeepWorkspaces      = KeepFailed
)

// Upper bounds a suite may set; they guard against a typo that would start thousands of runs.
const (
	maxRepeat              = 1000
	maxParallel            = 64
	maxCheckOutputMaxBytes = 16 << 20
	maxCases               = 10_000
)

// Workspace retention after a case.
const (
	KeepAlways = "always"
	KeepFailed = "failed"
	KeepNever  = "never"
)

var (
	caseNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	suiteNamePattern = caseNamePattern
)

// Suite is a parsed eval suite file.
type Suite struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	// SessionRoot is the id of the session root the case sessions are written to; empty
	// uses the first root of the root setting.
	SessionRoot string `json:"session_root,omitempty"`
	// Session is merged into every case's session file (scope, settings, category); the
	// runner sets schema_version, cwd and description itself.
	Session             map[string]json.RawMessage `json:"session,omitempty"`
	Profile             string                     `json:"profile,omitempty"`
	Model               string                     `json:"model,omitempty"`
	TimeoutMS           int64                      `json:"timeout_ms,omitempty"`
	CheckTimeoutMS      int64                      `json:"check_timeout_ms,omitempty"`
	CheckOutputMaxBytes int                        `json:"check_output_max_bytes,omitempty"`
	Repeat              int                        `json:"repeat,omitempty"`
	Parallel            int                        `json:"parallel,omitempty"`
	KeepWorkspaces      string                     `json:"keep_workspaces,omitempty"`
	// WorkDir is where workspaces are created, relative to the suite file; empty uses the
	// system temporary directory.
	WorkDir string `json:"work_dir,omitempty"`
	// Shell runs setup and check commands: the command line is appended as the last argument.
	Shell []string `json:"shell,omitempty"`
	Cases []Case   `json:"cases"`

	// dir is the folder of the suite file; relative paths in the suite are resolved from it.
	dir string
}

// Case is one task of a suite.
type Case struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Workspace is a folder copied into the fresh workspace, relative to the suite file;
	// empty starts from an empty folder.
	Workspace string `json:"workspace,omitempty"`
	// Setup commands run in the workspace before the agent; a failure makes the case an error.
	Setup []string `json:"setup,omitempty"`
	// Checks run in the workspace after the agent; each must exit 0 for the case to pass.
	Checks []string `json:"checks,omitempty"`
	// ExpectText are regular expressions the final answer must each match.
	ExpectText []string `json:"expect_text,omitempty"`
	Profile    string   `json:"profile,omitempty"`
	Model      string   `json:"model,omitempty"`
	TimeoutMS  int64    `json:"timeout_ms,omitempty"`

	expect []*regexp.Regexp
}

// defaultShell runs a command line with the POSIX shell.
var defaultShell = []string{"sh", "-c"}

// LoadSuite reads and validates a suite file and fills in its defaults.
func LoadSuite(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the suite: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	suite, err := ParseSuite(raw, filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return suite, nil
}

// ParseSuite parses a suite whose relative paths are relative to dir.
func ParseSuite(raw []byte, dir string) (*Suite, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var suite Suite
	if err := decoder.Decode(&suite); err != nil {
		return nil, fmt.Errorf("invalid suite: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("invalid suite: more than one JSON value")
	}
	suite.dir = dir
	if err := suite.normalize(); err != nil {
		return nil, err
	}
	return &suite, nil
}

func (s *Suite) normalize() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if !suiteNamePattern.MatchString(s.Name) {
		return fmt.Errorf("name must match %s", suiteNamePattern)
	}
	for _, key := range []string{"schema_version", "cwd", "description"} {
		if _, set := s.Session[key]; set {
			return fmt.Errorf("session.%s is set by the runner and must not be given", key)
		}
	}
	if s.Repeat == 0 {
		s.Repeat = DefaultRepeat
	}
	if s.Repeat < 1 || s.Repeat > maxRepeat {
		return fmt.Errorf("repeat must be between 1 and %d", maxRepeat)
	}
	if s.Parallel == 0 {
		s.Parallel = DefaultParallel
	}
	if s.Parallel < 1 || s.Parallel > maxParallel {
		return fmt.Errorf("parallel must be between 1 and %d", maxParallel)
	}
	if s.TimeoutMS == 0 {
		s.TimeoutMS = DefaultRunTimeoutMS
	}
	if s.CheckTimeoutMS == 0 {
		s.CheckTimeoutMS = DefaultCheckTimeoutMS
	}
	if s.TimeoutMS < 0 || s.CheckTimeoutMS < 0 {
		return errors.New("timeout_ms and check_timeout_ms must be positive")
	}
	if s.CheckOutputMaxBytes == 0 {
		s.CheckOutputMaxBytes = DefaultCheckOutputMaxBytes
	}
	if s.CheckOutputMaxBytes < 0 || s.CheckOutputMaxBytes > maxCheckOutputMaxBytes {
		return fmt.Errorf("check_output_max_bytes must be between 1 and %d", maxCheckOutputMaxBytes)
	}
	if s.KeepWorkspaces == "" {
		s.KeepWorkspaces = DefaultKeepWorkspaces
	}
	switch s.KeepWorkspaces {
	case KeepAlways, KeepFailed, KeepNever:
	default:
		return fmt.Errorf("keep_workspaces must be always, failed or never")
	}
	if len(s.Shell) == 0 {
		s.Shell = append([]string(nil), defaultShell...)
	}
	if s.WorkDir != "" {
		s.WorkDir = s.resolve(s.WorkDir)
	}
	if len(s.Cases) == 0 || len(s.Cases) > maxCases {
		return fmt.Errorf("cases must list between 1 and %d cases", maxCases)
	}
	seen := map[string]bool{}
	for index := range s.Cases {
		one := &s.Cases[index]
		if !caseNamePattern.MatchString(one.Name) {
			return fmt.Errorf("cases[%d].name must match %s", index, caseNamePattern)
		}
		if seen[one.Name] {
			return fmt.Errorf("cases[%d].name %q is used twice", index, one.Name)
		}
		seen[one.Name] = true
		if strings.TrimSpace(one.Prompt) == "" {
			return fmt.Errorf("case %q: prompt must not be empty", one.Name)
		}
		if one.TimeoutMS < 0 {
			return fmt.Errorf("case %q: timeout_ms must be positive", one.Name)
		}
		if one.Workspace != "" {
			one.Workspace = s.resolve(one.Workspace)
			info, err := os.Stat(one.Workspace)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("case %q: workspace %s is not a folder", one.Name, one.Workspace)
			}
		}
		for _, pattern := range one.ExpectText {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("case %q: expect_text %q: %w", one.Name, pattern, err)
			}
			one.expect = append(one.expect, compiled)
		}
		if len(one.Checks) == 0 && len(one.ExpectText) == 0 {
			return fmt.Errorf("case %q: give checks or expect_text, or the case cannot fail", one.Name)
		}
	}
	return nil
}

func (s *Suite) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.dir, path)
}

// profile, model and timeout of a case: its own value, else the suite's.
func (s *Suite) profileOf(c *Case) string {
	if c.Profile != "" {
		return c.Profile
	}
	return s.Profile
}

func (s *Suite) modelOf(c *Case) string {
	if c.Model != "" {
		return c.Model
	}
	return s.Model
}

func (s *Suite) timeoutOf(c *Case) int64 {
	if c.TimeoutMS > 0 {
		return c.TimeoutMS
	}
	return s.TimeoutMS
}
