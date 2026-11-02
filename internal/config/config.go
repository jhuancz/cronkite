// Package config loads, defaults and validates the operator configuration.
//
// Two rules govern this package:
//   - Unknown keys are errors. A typo in "older_then" must not silently fall
//     back to a default and delete the wrong files.
//   - Validation collects every problem instead of failing on the first one,
//     so an operator fixes the file in one pass.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jhuancz/cronkite/internal/bytesx"
	"github.com/jhuancz/cronkite/internal/durationx"
)

// current is the only supported configuration schema version.
const current = 1

// TypeBackup is the task type whose fields this package validates beyond the
// ones every task shares. It is named here because a backup task's destination
// and retention are configuration, so their defaults and their problems have to
// be reported with the rest of the file instead of surfacing one run later.
const TypeBackup = "backup"

// TypeScript is the task type that runs an operator-supplied script file. It is
// named here for the same reason as TypeBackup: its interpreter, its file and
// its timeout are configuration, and a wrong one has to be a problem at load
// time and not a surprise in a scheduled run.
const TypeScript = "script"

// ScriptInterpreters lists the interpreters a script task may name. The list is
// closed on purpose: cronkite knows how it starts these three and how it stops
// them, and an interpreter nobody tested is a configuration mistake rather than
// a runtime experiment.
var ScriptInterpreters = []string{"bash", "sh", "powershell"}

// IsScriptInterpreter reports whether name is one of ScriptInterpreters.
func IsScriptInterpreter(name string) bool {
	for _, known := range ScriptInterpreters {
		if name == known {
			return true
		}
	}
	return false
}

// Defaults for the fields only a backup task uses. They are exported so the
// task that consumes them falls back on the same numbers this package validates.
const (
	DefaultRetentionDaily  = 7
	DefaultRetentionWeekly = 4
	DefaultSamplePercent   = 10
	// DefaultMaxBackupBytes is the most one snapshot may copy when the file does
	// not say. It bounds what a mistyped backup can spend, in the spirit of
	// run.safety: a first snapshot of a 200 GiB tree is a decision an operator
	// has to make, not one a default should make for them.
	DefaultMaxBackupBytes = "5GiB"

	// DefaultScriptTimeout bounds one script run when the task does not say. A
	// script that hangs is a script that holds the run lock, so the cap is part
	// of the contract and not optional.
	DefaultScriptTimeout = "5m"
)

// Config is the parsed configuration file.
type Config struct {
	Version int    `yaml:"version"`
	Run     Run    `yaml:"run"`
	Report  Report `yaml:"report"`
	Notify  Notify `yaml:"notify"`
	Tasks   []Task `yaml:"tasks"`
}

// Run holds execution-wide settings.
type Run struct {
	StateDir string `yaml:"state_dir"`
	LockFile string `yaml:"lock_file"`
	Timeout  string `yaml:"timeout"`
	Safety   Safety `yaml:"safety"`
}

// Safety are the hard ceilings that stop a misconfiguration from wiping a
// filesystem. They are checked against the whole planned run, before anything
// is deleted.
type Safety struct {
	MaxDeleteFiles int    `yaml:"max_delete_files"`
	MaxDeleteBytes string `yaml:"max_delete_bytes"`
}

// Report configures where run reports are written and how much detail they
// carry.
type Report struct {
	Dir               string `yaml:"dir"`
	MaxActionsPerTask int    `yaml:"max_actions_per_task"`
}

// Notify configures alert delivery. Fields are parsed in v0.1.0 and used from
// v0.2.0, so configurations stay forward compatible.
type Notify struct {
	Enabled  bool     `yaml:"enabled"`
	On       []string `yaml:"on"`
	Throttle string   `yaml:"throttle"`
	Telegram Telegram `yaml:"telegram"`
}

// Telegram delivers alerts through the Bot API. The token is never stored in
// the configuration file: it is read from TokenEnv.
type Telegram struct {
	ChatID   string `yaml:"chat_id"`
	TokenEnv string `yaml:"token_env"`
}

// Task is one configured maintenance task.
type Task struct {
	Name            string   `yaml:"name"`
	Type            string   `yaml:"type"`
	Enabled         *bool    `yaml:"enabled"`
	Roots           []string `yaml:"roots"`
	OlderThan       string   `yaml:"older_than"`
	MaxDepth        *int     `yaml:"max_depth"`
	RemoveEmptyDirs *bool    `yaml:"remove_empty_dirs"`
	Protect         []string `yaml:"protect"`

	// Destination is where a backup task publishes its snapshots. It is an
	// output path and not a root: nothing is ever deleted inside it except a
	// whole snapshot cronkite wrote itself, and it must sit outside the task's
	// source, which is what roots describes.
	Destination string `yaml:"destination"`
	// Retention bounds the snapshot history a backup task keeps.
	Retention Retention `yaml:"retention"`
	// VerifySamplePercent is the share of a snapshot's files re-hashed against
	// the source after it is written. Absent means DefaultSamplePercent, and at
	// least one file is always verified.
	VerifySamplePercent *int `yaml:"verify_sample_percent"`
	// MaxBackupBytes is the ceiling on the bytes one run may copy. Bytes the
	// previous snapshot supplies are not counted against it: a relink stores
	// nothing new, so charging it would refuse exactly the incremental snapshots
	// this task exists to take. A run whose plan would copy more is refused
	// before anything is written. Absent means DefaultMaxBackupBytes.
	MaxBackupBytes string `yaml:"max_backup_bytes"`

	// Interpreter is the program a script task runs its file with: one of
	// ScriptInterpreters. File is the script itself, which is a file the
	// operator names - cronkite never downloads or generates it - so its path is
	// required and absolute, like a root or a backup destination.
	Interpreter string `yaml:"interpreter"`
	File        string `yaml:"file"`
	// Timeout bounds one script run. Absent means DefaultScriptTimeout. A run
	// past it is killed as a process group, so nothing the script started
	// outlives the run.
	Timeout string `yaml:"timeout"`
	// Args are handed to the script after the file, in order and unquoted.
	Args []string `yaml:"args"`

	// UnresolvedRoots holds roots whose path depends on an environment
	// variable that is not set here. One configuration is meant to be shared
	// between Linux and Windows, so "%TEMP%" on Linux is inapplicable rather
	// than invalid.
	UnresolvedRoots []string `yaml:"-"`
}

// Retention is how much snapshot history a backup task keeps: the newest
// snapshot of each UTC day for Daily days and of each ISO week for Weekly weeks.
// A zero value turns that rule off by naming the other one; both zero is
// refused, because a policy that keeps nothing deletes everything.
type Retention struct {
	Daily  *int `yaml:"daily"`
	Weekly *int `yaml:"weekly"`
}

// IsEnabled reports whether the task should run. Absent means enabled.
func (t Task) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// ValidationError reports every problem found in a configuration file.
type ValidationError struct {
	Path     string
	Problems []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	if e.Path != "" {
		fmt.Fprintf(&b, "invalid configuration %s:", e.Path)
	} else {
		b.WriteString("invalid configuration:")
	}
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p)
	}
	return b.String()
}

// Load reads, expands, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			ve.Path = path
		}
		return nil, err
	}
	return cfg, nil
}

// Parse decodes configuration bytes, rejecting unknown keys.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &ValidationError{Problems: []string{"file is empty"}}
		}
		return nil, &ValidationError{Problems: []string{err.Error()}}
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalize applies defaults, expands every operator-supplied path and
// validates the result.
func (c *Config) normalize() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Version == 0 {
		add("version is required (expected %d)", current)
	} else if c.Version != current {
		add("unsupported version %d (expected %d)", c.Version, current)
	}

	c.Run.StateDir = expand(firstNonEmpty(c.Run.StateDir, defaultStateDir()))
	c.Run.LockFile = expand(firstNonEmpty(c.Run.LockFile, filepath.Join(c.Run.StateDir, "cronkite.lock")))
	c.Run.Timeout = firstNonEmpty(c.Run.Timeout, "30m")
	if _, err := durationx.Parse(c.Run.Timeout); err != nil {
		add("run.timeout: %v", err)
	}

	if c.Run.Safety.MaxDeleteFiles <= 0 {
		c.Run.Safety.MaxDeleteFiles = 500
	}
	c.Run.Safety.MaxDeleteBytes = firstNonEmpty(c.Run.Safety.MaxDeleteBytes, "5GiB")
	if _, err := bytesx.Parse(c.Run.Safety.MaxDeleteBytes); err != nil {
		add("run.safety.max_delete_bytes: %v", err)
	}

	c.Report.Dir = expand(firstNonEmpty(c.Report.Dir, filepath.Join(c.Run.StateDir, "reports")))
	if c.Report.MaxActionsPerTask < 0 {
		add("report.max_actions_per_task: must not be negative")
	}
	if c.Report.MaxActionsPerTask == 0 {
		c.Report.MaxActionsPerTask = 1000
	}

	if len(c.Notify.On) == 0 {
		c.Notify.On = []string{"failure", "partial"}
	}
	for _, on := range c.Notify.On {
		switch on {
		case "failure", "partial", "always", "never":
		default:
			add("notify.on: unknown event %q (want failure, partial, always or never)", on)
		}
	}
	c.Notify.Throttle = firstNonEmpty(c.Notify.Throttle, "15m")
	if _, err := durationx.Parse(c.Notify.Throttle); err != nil {
		add("notify.throttle: %v", err)
	}
	c.Notify.Telegram.TokenEnv = firstNonEmpty(c.Notify.Telegram.TokenEnv, "CRONKITE_TELEGRAM_TOKEN")
	if c.Notify.Enabled && c.Notify.Telegram.ChatID == "" {
		add("notify.telegram.chat_id is required when notify.enabled is true")
	}

	seen := map[string]bool{}
	for i := range c.Tasks {
		t := &c.Tasks[i]
		where := fmt.Sprintf("tasks[%d]", i)
		if t.Name == "" {
			add("%s: name is required", where)
		} else {
			if !validName(t.Name) {
				add("%s: name %q must be a slug (letters, digits, dash, underscore)", where, t.Name)
			}
			if seen[t.Name] {
				add("%s: duplicate task name %q", where, t.Name)
			}
			seen[t.Name] = true
			where = "tasks[" + t.Name + "]"
		}
		if t.Type == "" {
			add("%s: type is required", where)
		}
		expanded := make([]string, 0, len(t.Roots))
		for _, root := range t.Roots {
			if root == "" {
				add("%s: roots contains an empty path", where)
				continue
			}
			resolved := expand(root)
			if resolved == "" {
				t.UnresolvedRoots = append(t.UnresolvedRoots, root)
				continue
			}
			if !filepath.IsAbs(resolved) {
				add("%s: root %q must be absolute", where, resolved)
				continue
			}
			expanded = append(expanded, resolved)
		}
		t.Roots = expanded
		// A script names its own file and decides for itself what it touches, so
		// requiring roots would ask for a scope cronkite does not enforce on it.
		if len(t.Roots) == 0 && len(t.UnresolvedRoots) == 0 && t.Type != TypeScript {
			add("%s: roots is required", where)
		}

		// A backup's destination is an output path: it is expanded and required
		// here for the same reason a root is, and it is deliberately not folded
		// into roots, which describe what may be deleted inside.
		if t.Type == TypeBackup {
			switch dest := expand(t.Destination); {
			case t.Destination == "":
				add("%s: destination is required for a backup task", where)
			case dest == "":
				add("%s: destination %q depends on an environment variable that is not set here", where, t.Destination)
			case !filepath.IsAbs(dest):
				add("%s: destination %q must be absolute", where, dest)
			default:
				t.Destination = dest
			}
			if t.Retention.Daily == nil {
				t.Retention.Daily = intPtr(DefaultRetentionDaily)
			}
			if t.Retention.Weekly == nil {
				t.Retention.Weekly = intPtr(DefaultRetentionWeekly)
			}
			if t.VerifySamplePercent == nil {
				t.VerifySamplePercent = intPtr(DefaultSamplePercent)
			}
			// The ceiling is required for every backup task, because the delete
			// ceilings of run.safety describe removals and cannot stand in for it.
			t.MaxBackupBytes = firstNonEmpty(t.MaxBackupBytes, DefaultMaxBackupBytes)
			if _, err := t.BackupBytes(); err != nil {
				add("%s: %v", where, err)
			}
		}
		// A script's file is read by an interpreter, so the path is checked the
		// way a root is: absolute, environment-expandable, and never a guess made
		// at run time. Whether it exists is a run-time question the task answers
		// before starting anything.
		if t.Type == TypeScript {
			interpreters := strings.Join(ScriptInterpreters, ", ")
			switch {
			case t.Interpreter == "":
				add("%s: interpreter is required for a script task (want one of: %s)", where, interpreters)
			case !IsScriptInterpreter(t.Interpreter):
				add("%s: interpreter %q is not supported (want one of: %s)", where, t.Interpreter, interpreters)
			}

			switch file := expand(t.File); {
			case t.File == "":
				add("%s: file is required for a script task", where)
			case file == "":
				add("%s: file %q depends on an environment variable that is not set here", where, t.File)
			case !filepath.IsAbs(file):
				add("%s: file %q must be absolute", where, file)
			default:
				t.File = file
			}

			t.Timeout = firstNonEmpty(t.Timeout, DefaultScriptTimeout)
			if d, err := durationx.Parse(t.Timeout); err != nil {
				add("%s: timeout: %v", where, err)
			} else if d <= 0 {
				add("%s: timeout must be positive (a run that expires at once cannot do anything)", where)
			}
		}

		if t.Retention.Daily != nil && *t.Retention.Daily < 0 {
			add("%s: retention.daily must not be negative (0 turns the daily rule off)", where)
		}
		if t.Retention.Weekly != nil && *t.Retention.Weekly < 0 {
			add("%s: retention.weekly must not be negative (0 turns the weekly rule off)", where)
		}
		if t.Retention.Daily != nil && t.Retention.Weekly != nil && *t.Retention.Daily == 0 && *t.Retention.Weekly == 0 {
			add("%s: retention keeps no snapshot: retention.daily and retention.weekly are both 0", where)
		}
		if t.VerifySamplePercent != nil {
			if p := *t.VerifySamplePercent; p < 0 || p > 100 {
				add("%s: verify_sample_percent must be between 0 and 100, got %d", where, p)
			}
		}

		if t.OlderThan != "" {
			d, err := durationx.Parse(t.OlderThan)
			if err != nil {
				add("%s: older_than: %v", where, err)
			} else if d < 0 {
				add("%s: older_than must not be negative", where)
			}
		}
		if t.MaxDepth != nil && *t.MaxDepth < 0 {
			add("%s: max_depth must not be negative (0 means unlimited)", where)
		}
		for _, p := range t.Protect {
			if p == "" {
				add("%s: protect contains an empty pattern", where)
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return &ValidationError{Problems: problems}
	}
	return nil
}

// SafetyBytes returns max_delete_bytes in bytes. Call after Load.
func (c *Config) SafetyBytes() int64 {
	n, err := bytesx.Parse(c.Run.Safety.MaxDeleteBytes)
	if err != nil {
		return 0
	}
	return n
}

// BackupBytes returns max_backup_bytes in bytes, applying the documented default
// when the file does not set one. Call after Load.
//
// A value that cannot be parsed or that is not positive is an error rather than
// a silent zero: a ceiling read as zero would refuse every snapshot, and a
// ceiling read as unlimited would enforce nothing.
func (t Task) BackupBytes() (int64, error) {
	limit := firstNonEmpty(t.MaxBackupBytes, DefaultMaxBackupBytes)
	n, err := bytesx.Parse(limit)
	if err != nil {
		return 0, fmt.Errorf("max_backup_bytes: %w", err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("max_backup_bytes is %q: a ceiling of zero refuses every snapshot", limit)
	}
	return n, nil
}

// TaskNames returns the configured task names in file order.
func (c *Config) TaskNames() []string {
	names := make([]string, 0, len(c.Tasks))
	for _, t := range c.Tasks {
		names = append(names, t.Name)
	}
	return names
}

// defaultStateDir follows platform convention for persistent machine-local
// state, so an unprivileged run works without any configuration.
func defaultStateDir() string {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "cronkite")
		}
	} else if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "cronkite")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cronkite")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Local", "cronkite")
	}
	return filepath.Join(home, ".local", "state", "cronkite")
}

// expand resolves $VAR, ${VAR} and %VAR% references plus a leading ~.
func expand(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), string(filepath.Separator)))
		}
	}
	path = os.Expand(path, func(key string) string {
		if value, ok := os.LookupEnv(key); ok {
			return value
		}
		return ""
	})
	path = expandPercent(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

// expandPercent resolves Windows-style %VAR% references on every platform, so
// one configuration file works on Linux, macOS and Windows.
func expandPercent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		end := strings.IndexByte(s[i+1:], '%')
		if end < 0 {
			b.WriteByte(s[i])
			continue
		}
		key := s[i+1 : i+1+end]
		if value, ok := os.LookupEnv(key); ok {
			b.WriteString(value)
		}
		i += end + 1
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func intPtr(v int) *int { return &v }

func validName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
