// Package tasks turns configuration into runnable tasks.
//
// The switch is deliberately explicit rather than a global registry: it makes
// the supported types greppable and keeps construction side-effect free. When
// external plugins arrive, they plug in here.
package tasks

import (
	"fmt"
	"strings"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/task"
	"github.com/jhuancz/cronkite/internal/tasks/backup"
	"github.com/jhuancz/cronkite/internal/tasks/cleanup"
	"github.com/jhuancz/cronkite/internal/tasks/script"
)

// Types lists the task types this build supports.
var Types = []string{config.TypeBackup, config.TypeScript, "logclean", "tempclean"}

// Build constructs one task from its configuration.
func Build(cfg config.Task) (task.Runner, error) {
	switch cfg.Type {
	case config.TypeBackup:
		return backup.New(cfg)
	case config.TypeScript:
		return script.New(cfg)
	case "logclean", "tempclean":
		return cleanup.New(cfg)
	default:
		return nil, fmt.Errorf("task %s: unknown type %q (available: %s)", cfg.Name, cfg.Type, strings.Join(Types, ", "))
	}
}
