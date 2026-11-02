# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

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
