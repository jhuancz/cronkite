// Package task defines the contract every maintenance task implements.
//
// The two-phase shape is the core design decision: a task first produces a
// Plan and only then, in apply mode, acts on it. A dry run is therefore not a
// separate code path that can drift from the real one; it is the same plan with
// the second phase skipped.
package task

import (
	"context"
	"fmt"
	"time"

	"github.com/jhuancz/cronkite/internal/report"
)

// Env is the immutable context handed to every task.
type Env struct {
	// Mode is plan or apply.
	Mode report.Mode
	// Now is the reference time for age comparisons. Injected so tests are
	// deterministic and a long run does not use a moving cutoff.
	Now time.Time
}

// Runner is one maintenance task.
type Runner interface {
	Name() string
	Type() string
	Plan(ctx context.Context, env Env) (*Plan, error)
	Apply(ctx context.Context, plan *Plan, env Env) (*Result, error)
	Close() error
}

// Action is one intended filesystem change.
type Action struct {
	Path   string
	Kind   report.ActionKind
	Size   int64
	Reason string
}

// Plan is everything a task intends to do, computed before anything changes.
type Plan struct {
	Actions []Action
	// Skipped are candidates deliberately left untouched, with the reason
	// recorded on each one.
	Skipped []Action
	// Dirs are candidate directories for removal, deepest first. Only used by
	// apply mode, and only when empty.
	Dirs []string
	// Notes are operator-facing explanations: missing roots, unreadable
	// directories, and similar conditions that are not failures.
	Notes []string
}

// Files is the number of planned file removals.
func (p *Plan) Files() int {
	n := 0
	for _, a := range p.Actions {
		if a.Kind == report.ActionDelete {
			n++
		}
	}
	return n
}

// Bytes is the total size of every planned action, whatever it does. It answers
// "how much work is this", not "how much data will this free".
func (p *Plan) Bytes() int64 {
	var total int64
	for _, a := range p.Actions {
		total += a.Size
	}
	return total
}

// DeleteBytes is the size planned for deletion. The safety ceilings are about
// destruction, so they must count this and never the whole plan: a backup that
// copies six gigabytes destroys nothing, and refusing it with a message about
// removing six gigabytes would be both wrong and alarming.
func (p *Plan) DeleteBytes() int64 {
	var total int64
	for _, a := range p.Actions {
		switch a.Kind {
		case report.ActionDelete, report.ActionRmdir:
			total += a.Size
		}
	}
	return total
}

// Result is what actually happened in apply mode.
type Result struct {
	Done        []Action
	Failed      []Failure
	Skipped     []Action
	DirsRemoved int
}

// Failure is one path that could not be handled.
type Failure struct {
	Path string
	Err  error
}

// String makes failures readable in logs.
func (f Failure) String() string {
	return fmt.Sprintf("%s: %v", f.Path, f.Err)
}
