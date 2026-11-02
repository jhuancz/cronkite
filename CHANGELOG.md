# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet.

## [0.2.0] - 2026-11-02

### Added

- `backup` task type: one snapshot per run is a plain directory that restores with
  `cp`, unchanged files are hardlinked from the previous snapshot so the
  filesystem deduplicates, and a manifest with a digest per file is the index.
- Snapshot verification: published bytes are re-hashed for a sample, and the
  *source* is re-hashed for every relinked entry, because an inherited digest is a
  claim until something checks it. Coverage is reported as it was, never as hoped.
- Retention by day and ISO week, whole snapshots only, never the newest one and
  never one whose manifest cannot be read.
- `script` task type: a bash, sh or PowerShell file runs inside the run, under the
  same lock, report and rehearsal. The script receives `CRONKITE_RUN_ID`,
  `CRONKITE_MODE`, `CRONKITE_DRY_RUN` and `CRONKITE_TASK`, its output is captured
  into the report, and its timeout kills the process group so nothing it started
  outlives the run.
- Telegram alerts: an event filter, stateful throttling that never suppresses an
  escalation, a token read only from the environment and redacted from every
  error, and a notifier that cannot change a run's status or exit code.
- `examples/maintenance.sh` and `examples/maintenance.ps1`, written to be copied:
  both honour the dry run and exit non-zero instead of failing quietly.
- Installers and scheduler wrappers (`scripts/install.sh`, `scripts/install.ps1`,
  `scripts/cronkite-run.sh`, `scripts/cronkite-run.ps1`) plus
  [docs/scheduling.md](docs/scheduling.md) for cron, systemd timers and Task
  Scheduler. The wrappers map a locked run to success so an overlap is not an
  incident.

### Changed

- Plan mode tells the truth about work: the safety ceilings count only deletion
  actions, so a backup that copies gigabytes is no longer measured as if it
  destroyed them, and a plan's verb and counts come from the actions it contains.
- The report separates what was freed from what was written and relinked. A backup
  is never described as a deletion in the tool whose artifact is the audit report.
- Snapshots record the directory skeleton, so a restore reproduces the tree and not
  just its files.
- `task.Env` carries the run id and `task.Result` carries notes, so a script can be
  matched against the report that describes it and a task with no actions can still
  explain itself. `task.ErrCeiling` moved into the contract so the engine never
  imports a concrete task package.
- CI runs a `shellcheck` job over the shell layer, because those scripts run as root
  on machines that delete files.

### Fixed

- Sixteen defects found by review and by independent verification, in two rounds.
  The ones worth naming: a relink that trusted size and mtime silently published
  stale bytes; a symlinked directory inside an older snapshot could pull content
  from outside the configured root into a new one; publishing had no durability, so
  a power loss could leave a snapshot that looked complete; and a rehearsal of a
  script was a silent no-op, which made a destructive script look safe in plan mode.
- The cron and Task Scheduler wrappers returned 1 for every input on bash 3.2, where
  expanding an empty array under `set -u` is an error.
- The installers printed a schedule whose wrapper looked for the binary at the
  default location, so an install under any other prefix produced a job that never
  ran.

### Fixed (earlier, still unreleased)



- Checksum verification instructions now work on macOS, where `sha256sum` does
  not exist (`shasum -a 256`). The broken command also shipped in the release
  notes published by GoReleaser.
- The release workflow refuses a tag that is not reachable from `main`, so a tag
  on a side branch or a local experiment cannot publish binaries.
- Third-party actions are pinned to commit SHAs: the release job holds a write
  token and must not execute whatever a moved major tag happens to point at.
- The release job runs the test suite with `-race`, matching CI, so a release is
  never built on a weaker signal than the one that gated the merge.

## [0.1.0] - 2026-11-02

### Added

- Two-phase engine: every task plans before anything is applied, so a rehearsal
  runs the same code path as a real run.
- `logclean` and `tempclean` task types, sharing one implementation with
  different presets (30d/4 levels and 7d/8 levels).
- Path confinement through `os.Root`: a removal cannot leave a configured root,
  and removing a symlink removes the link, never its target.
- Safety ceilings on deleted files and bytes per run, enforced against the whole
  plan before the first deletion. `--force` bypasses them and is recorded in the
  report.
- Cross-platform non-blocking run lock. Overlapping runs exit `3` and do nothing.
- Versioned JSON run report (`schema: 1`), written atomically per run and
  mirrored to `latest.json`, with complete counters and a capped action list.
- Human-readable run summary for terminals.
- Configuration loader with strict unknown-key rejection, collected validation
  problems, env/`%VAR%`/`~` path expansion, and platform-aware defaults.
- Protection patterns, matched against file names or trailing paths, inherited by
  subtrees and re-checked at removal time.
- CLI: `check`, `plan`, `run [--apply]`, `report`, `version`, with a documented
  exit-code contract.
- CI on Linux, macOS and Windows: gofmt, vet, and the full test suite with the
  race detector.
- A tag-triggered release pipeline: static binaries for linux, darwin and
  windows on amd64 and arm64, archives with the docs, and `checksums.txt`.

### Notes

- The report schema is additive-only within `schema: 1`.
- Task types are `logclean` and `tempclean`; `backup` and `script` are v0.2.0.
- Alerting is parsed and validated in configuration but not delivered yet.

## Release history

| Version | Date | Scope |
| --- | --- | --- |
| v0.1.0 | 2026-11-02 | Engine, cleanup tasks, locking, safety ceilings, reports |
