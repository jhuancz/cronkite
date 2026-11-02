// Package report defines the versioned, machine-readable record of a run.
//
// The report is the product's main artifact: every run produces one, in plan
// mode and in apply mode, with the same shape. That symmetry is what makes a
// dry run trustworthy, and what lets monitoring consume cronkite without
// scraping text.
package report

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jhuancz/cronkite/internal/version"
)

// Schema is the report format version. Bump it when a field is removed or its
// meaning changes, never for additive changes.
const Schema = 1

// Mode distinguishes a rehearsal from a run that touched the filesystem.
type Mode string

const (
	ModePlan  Mode = "plan"
	ModeApply Mode = "apply"
)

// Status is the outcome of a run or of a single task.
type Status string

const (
	StatusOK      Status = "ok"
	StatusPartial Status = "partial"
	StatusFailed  Status = "failed"
	StatusRefused Status = "refused"
	StatusSkipped Status = "skipped"
)

// ActionKind is what cronkite did, or would do, to a path.
type ActionKind string

const (
	ActionDelete ActionKind = "delete"
	ActionRmdir  ActionKind = "rmdir"
)

// Report is one complete run.
type Report struct {
	Schema     int       `json:"schema"`
	RunID      string    `json:"run_id"`
	Tool       Tool      `json:"tool"`
	Mode       Mode      `json:"mode"`
	Host       Host      `json:"host"`
	ConfigPath string    `json:"config_path"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS int64     `json:"duration_ms"`
	Status     Status    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Totals     Totals    `json:"totals"`
	Tasks      []Task    `json:"tasks"`
	Notes      []string  `json:"notes,omitempty"`
}

// Tool identifies the binary that produced the report.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Host identifies the machine the run happened on.
type Host struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Totals are additive counters. They are always complete, even when the
// per-task action list is truncated.
type Totals struct {
	FilesRemoved int   `json:"files_removed"`
	DirsRemoved  int   `json:"dirs_removed"`
	BytesFreed   int64 `json:"bytes_freed"`
	Errors       int   `json:"errors"`
	Skipped      int   `json:"skipped"`
}

// Add accumulates other into t.
func (t Totals) Add(other Totals) Totals {
	return Totals{
		FilesRemoved: t.FilesRemoved + other.FilesRemoved,
		DirsRemoved:  t.DirsRemoved + other.DirsRemoved,
		BytesFreed:   t.BytesFreed + other.BytesFreed,
		Errors:       t.Errors + other.Errors,
		Skipped:      t.Skipped + other.Skipped,
	}
}

// Task is the result of one configured task.
type Task struct {
	Name             string    `json:"name"`
	Type             string    `json:"type"`
	Status           Status    `json:"status"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	DurationMS       int64     `json:"duration_ms"`
	Planned          int       `json:"planned"`
	PlannedBytes     int64     `json:"planned_bytes,omitempty"`
	Totals           Totals    `json:"totals"`
	Actions          []Action  `json:"actions,omitempty"`
	ActionsTruncated bool      `json:"actions_truncated,omitempty"`
	Errors           []Failure `json:"errors,omitempty"`
	Notes            []string  `json:"notes,omitempty"`
}

// Action is one intended or performed filesystem change.
type Action struct {
	Path   string     `json:"path"`
	Action ActionKind `json:"action"`
	Size   int64      `json:"size,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// Failure is one path cronkite could not handle.
type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// New builds an empty report for a run starting now.
func New(mode Mode, configPath string) *Report {
	hostname, _ := os.Hostname()
	now := time.Now().UTC()
	return &Report{
		Schema:     Schema,
		RunID:      NewRunID(now),
		Tool:       Tool{Name: "cronkite", Version: version.Version, Commit: version.Commit},
		Mode:       mode,
		Host:       Host{Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH},
		ConfigPath: configPath,
		StartedAt:  now,
		Status:     StatusOK,
		Tasks:      []Task{},
	}
}

// NewRunID returns a sortable, collision-resistant run identifier.
func NewRunID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return now.UTC().Format("20060102T150405Z")
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Close stamps the end of the run and its duration.
func (r *Report) Close(now time.Time) {
	r.FinishedAt = now.UTC()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
}

// Write encodes the report as indented JSON.
func (r *Report) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// WriteFile writes the report atomically, so a monitoring agent never reads a
// half-written file.
func (r *Report) WriteFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".report-*.json")
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := r.Write(tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}

// ReadFile decodes a report written by cronkite.
func ReadFile(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode report %s: %w", path, err)
	}
	return &r, nil
}
