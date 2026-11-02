# Architecture

## Layout

```
cmd/cronkite/          CLI: version, check, plan, run, report
internal/config/       Load, default, expand and validate the configuration
internal/report/       Versioned run report: JSON schema plus human rendering
internal/fsx/          Path confinement (os.Root), protect patterns, walking
internal/lock/         Cross-platform non-blocking run lock
internal/task/         The Runner contract and Plan/Result types
internal/tasks/        Configuration to task construction
internal/tasks/cleanup/ One implementation behind logclean and tempclean
internal/engine/       Orchestration: plan all, enforce ceilings, apply
internal/exitcode/     The published exit-code contract
internal/bytesx/       "5GiB" parsing and formatting
internal/durationx/    "30d" parsing
internal/version/      Build identity injected at link time
```

## The flow of a run

```
config ──► select tasks ──► Plan (every task) ──► safety ceilings ──► Apply ──► report
                                  │                                        │
                                  └──────────── identical in plan mode ────┘
```

1. **Select.** Enabled tasks, optionally narrowed with `-task`.
2. **Plan every task.** Each task walks its roots and returns the exact list of
   actions it intends to take, plus skips with reasons and notes.
3. **Enforce ceilings.** Aggregate planned files and bytes against
   `run.safety`. Over budget means the whole run is refused: no task is applied.
4. **Apply.** Only in `--apply` mode, and only after step 3 allowed it.
5. **Report.** One JSON document, written atomically, plus the human summary.

The ordering in steps 2 and 3 is why `--force` is a deliberate, recorded act:
without an aggregate pre-check, task two would already be deleting while task
one was still being refused.

## The Runner contract

```go
type Runner interface {
	Name() string
	Type() string
	Plan(ctx context.Context, env Env) (*Plan, error)
	Apply(ctx context.Context, plan *Plan, env Env) (*Result, error)
	Close() error
}
```

`Env.Now` is injected rather than read from the clock, so age comparisons are
deterministic in tests and a long run does not drift its own cutoff.

`Plan` carries `Actions` (delete this, this size, this reason), `Skipped`
(candidates deliberately untouched, with the reason), `Dirs` (empty-directory
candidates, deepest first) and `Notes` (missing roots, unreadable
subdirectories). `Result` reports what actually happened.

Adding a task type means implementing this interface and adding one case to
`internal/tasks.Build` — the explicit switch keeps the supported surface
greppable.

## Script task type (v0.2.0)

Site-specific maintenance does not belong in a second cron entry with its own
logging, locking and alerting. The `script` task will run a shell or PowerShell
script as part of the run, under the same lock and the same report.

Contract for the script:

| Aspect | Rule |
| --- | --- |
| Interpreter | `bash`, `sh` or `powershell`, resolved from `PATH`. |
| Environment | `CRONKITE_RUN_ID`, `CRONKITE_MODE` (`plan`/`apply`), `CRONKITE_DRY_RUN` (`1`/`0`), `CRONKITE_TASK`. |
| Dry run | The script must honour `CRONKITE_DRY_RUN=1` and describe its intent without mutating. |
| Exit code | `0` success; non-zero is a task failure recorded in the report. |
| Timeout | `timeout` on the task; exceeding it kills the process tree and records a failure. |
| Output | Free-form text goes to the report as notes, capped by `report.max_actions_per_task`. |

Scripts run under the same ceilings and appear in the same report as native
tasks, which is the point: one run, one audit trail.

## Report schema versioning

`schema` is bumped only for removals or meaning changes. Additive fields do not
bump it. `internal/report.Schema` is the single source of truth, and every
report carries it so a consumer can refuse what it does not understand.

## Testing strategy

- `internal/*`: table tests per package, including the invariants that matter:
  an unknown config key is an error, a symlink cannot escape a root, the root
  itself cannot be removed.
- `cmd/cronkite`: `testscript` scenarios drive the **real binary** in a real
  temporary filesystem: a rehearsal changes nothing, a run removes only expired
  files, protection survives, and a refusal leaves every file in place.
- Exit codes and lock behaviour are asserted directly against `engine.Run` and
  the CLI entry point, not inferred from output text.

CI runs the whole suite on Linux, macOS and Windows.
