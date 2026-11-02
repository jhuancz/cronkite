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

## Backup task type (v0.2.0)

A `backup` task publishes incremental, verified snapshots of one source tree
under `destination/<UTC timestamp>/`.

What it does:

- **Writes a snapshot you can restore with `cp`.** A snapshot is a plain
directory holding a full copy of the tree plus a `manifest.json`, not an archive
and not a format. Restoring is `cp -a <snapshot>/. <where>/` (or the Windows
equivalent); no cronkite binary and no restore tool is involved, so a snapshot
outlives the tool that wrote it.
- **Reuses unchanged files with hardlinks.** A file is shared with the previous
snapshot when its size and modification time match the previous record and,
under the default checksum policy, its content digest still matches. The disk
holds one inode per distinct content, and every snapshot stays complete on its
own.
- **Verifies after writing.** A sample of the snapshot (`verify_sample_percent`,
default 10, at least one file) is re-hashed against the manifest, and every
relinked entry is re-read **from the source** and compared with the digest it
inherited. The second check is the only one that notices a file that changed
while its size and mtime stayed the same, and it is why a relink does not have
to re-read the whole tree while it copies.
- **Keeps whole snapshots.** Retention (`retention.daily`, `retention.weekly`)
counts snapshots of a UTC day and of an ISO week, not calendar age, deletes only
entire snapshot directories, and never the newest one. A snapshot whose index
cannot be read is reported and left alone rather than judged.
- **Refuses rather than guesses.** `max_backup_bytes` bounds the bytes one run
may copy; relinked bytes are not charged against it, because they store nothing
new. A plan above the ceiling is refused before anything is written, in a
rehearsal and in a run alike.
- **Reports the same shape in both modes.** `cronkite plan -task <name>` walks the
source, compares it with the newest readable snapshot using the writer's own
decision, and reports the bytes it would copy and the bytes it would reuse. It
creates nothing: no destination directory, no snapshot.

What it does not do:

- **No compression, encryption or off-site copy.** A snapshot is as large as the
distinct bytes it holds and lives where the destination points; shipping it
elsewhere is the operator's job.
- **No per-file selection.** The whole source tree goes in, minus symlinks,
special files, unreadable entries and a source file named `manifest.json` at the
root, which are reported as skipped with a reason.
- **No application-consistent view.** Files are read while the application keeps
writing, so a snapshot is crash-consistent, not transactionally consistent; a
file that changes during the copy is simply written again next run.
- **No hardlink guarantee.** A destination on another filesystem, or an
unprivileged Windows run, falls back to copying. Each fallback is recorded in
the report, never silent.

## Script task type

Site-specific maintenance does not belong in a second cron entry with its own
logging, locking and alerting. The `script` task runs a shell or PowerShell file
as part of the run, under the same lock, the same report and the same rehearsal.

The contract, which `examples/maintenance.sh` and `examples/maintenance.ps1`
implement:

| Aspect | Rule |
| --- | --- |
| Interpreter | `bash`, `sh` or `powershell`, resolved from `PATH` when the task is built, so a missing interpreter fails at start-up rather than at 03:00. |
| File | An absolute path to a regular file the operator named. cronkite never downloads or generates a script. |
| Environment | `CRONKITE_RUN_ID` (the id of the report this run writes, so a script's own log can be matched against it), `CRONKITE_MODE` (`plan`/`apply`), `CRONKITE_DRY_RUN` (`1`/`0`), `CRONKITE_TASK`. The inherited environment is passed through. |
| Dry run | A rehearsal **runs the script** with `CRONKITE_DRY_RUN=1` and records its output, because the only honest preview of an opaque script is the script's own. That makes the contract real: a script that ignores the variable can change things during a rehearsal, which is why the samples and this document state it. |
| Exit code | `0` is success; anything else is a task failure carrying the status and the tail of the output. The run continues with the next task, exactly as a failed deletion does. |
| Timeout | `timeout` on the task, 5m by default. Exceeding it kills the **process group**, not the direct child, so work the script spawned cannot outlive the run. On platforms without process groups only the direct child is killed, and the report says the task timed out. |
| Output | stdout and stderr are captured into the task's notes, capped at 8 KiB per stream with an explicit truncation note. Output is never printed to the terminal verbatim: the report is where it belongs. |
| Actions | None. cronkite did not measure any file change, so the report records a script through its notes and failures rather than inventing work in the totals. |
| Phases | `Plan` rehearses (dry run), `Apply` executes once. Apply mode does not rehearse, because the engine plans and then applies and the script would run twice. |

Scripts appear in the same report as native tasks, which is the point: one run,
one audit trail, one place to look after something went wrong.

## Notifier

Alerts are an outcome, not an activity log. `notify.on` decides what is worth a
message (`never`, `failure`, `partial`, `always`), a state file in the state
directory throttles repeats inside `notify.throttle`, and an escalation is never
throttled: silence during a new failure is the one thing an alert must not do.

Three invariants make it safe to enable:

- **It cannot change a run.** A Telegram outage, a wrong chat id or an expired
  token becomes a note in the report; the run's status and exit code keep
  describing the maintenance.
- **A rehearsal never sends.** Running `plan` must not post to a team channel.
- **The token only lives in the environment** variable the configuration names
  (`notify.telegram.token_env`), and it is redacted from every error, because an
  error message is the usual way a secret escapes.

Severity follows the documented exit codes: a locked run (exit 3) is silent, a
refused run (exit 4) alerts, and `failure` in the configuration means
partial-or-worse, because a run whose tasks failed is reported as `partial`.

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
