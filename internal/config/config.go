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
		if len(t.Roots) == 0 {
			add("%s: roots is required", where)
		}
		expanded := make([]string, 0, len(t.Roots))
		for _, root := range t.Roots {
			root = expand(root)
			if root == "" {
				add("%s: roots contains an empty path", where)
				continue
			}
			if !filepath.IsAbs(root) {
				add("%s: root %q must be absolute", where, root)
			}
			expanded = append(expanded, root)
		}
		t.Roots = expanded

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
