# cronkite

**Auditable maintenance runs for Linux and Windows servers.**

[![ci](https://github.com/jhuancz/cronkite/actions/workflows/ci.yml/badge.svg)](https://github.com/jhuancz/cronkite/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Log rotation, temp-file cleanup and incremental backups are the chores every
sysadmin automates with a pile of shell scripts. Those scripts mostly work, and
almost none of them can answer the two questions that matter after an incident:

1. **What exactly did it delete?** — not "it ran fine", but a list of paths.
2. **Can I prove it before it runs?** — a dry run that is the *same code path*
   as the real run, not a separate `echo`-only branch.

cronkite is built around those two answers. Every run produces one versioned
JSON report, in rehearsal and in production, with the same shape. The report is
the artifact, and monitoring consumes it instead of scraping console text.

```
$ cronkite plan
cronkite 0.1.0  run 20261102T102739Z-c55168f8  plan  status=ok
host web-01 (linux/amd64)  config /etc/cronkite/cronkite.conf

  TASK  TYPE      STATUS  PLANNED  WOULD REMOVE  SIZE     ERRORS
  logs  logclean  ok      412      412           38.4 MiB  -
  temp  tempclean ok      1203     1203          1.7 GiB   -

  total: 1615 files, 1.7 GiB would remove, 6 skipped, - errors

  nothing was changed. re-run with --apply to execute.
```

## Why not just a shell script

A script that deletes files is a loaded gun. The failure modes are well known and
all of them are silent:

- a config typo turns `/var/log/app` into `/var/log` and removes a year of audit
  trail;
- a symlink planted in `/tmp` points at `/etc`, and `rm -rf` follows it;
- two cron runs overlap and the second one reports a thousand phantom failures;
- the log says "done" while every single delete was denied by permissions.

cronkite addresses each of those explicitly, and documents the invariant behind
it. See [docs/safety.md](docs/safety.md).

## Safety model

| Invariant | How |
| --- | --- |
| Nothing changes unless asked | `plan` and `run` are rehearsals. Only `run --apply` mutates. |
| Deletion cannot leave a configured root | Every root is opened as an `os.Root` handle; kernel path resolution refuses `..` and escaping symlinks. |
| A typo cannot wipe a filesystem | Hard ceilings on files and bytes per run. Exceeding either refuses the *whole* run, before the first deletion. |
| Symlinks are never followed | The walk does not descend into them, and removing a link removes the link, never its target. |
| Runs cannot overlap | A non-blocking cross-platform lock. The second run exits `3` and touches nothing. |
| Silence is impossible | Every path is in the report as removed, skipped with a reason, or failed. |
| A broken privilege setup is loud | If a task could not remove *anything* because of permissions, that is a failure, not a quiet success. |

## Install

Every release publishes prebuilt archives for Linux, macOS and Windows on
amd64 and arm64, with `checksums.txt`. See the
[releases page](https://github.com/jhuancz/cronkite/releases) and
[docs/releasing.md](docs/releasing.md) for how to verify a download.

Or let Go resolve the module:

```sh
go install github.com/jhuancz/cronkite/cmd/cronkite@latest
```

Or build from source:

```sh
git clone https://github.com/jhuancz/cronkite
cd cronkite
make build
```

Requires Go 1.26 or newer to build. The resulting binary has no runtime
dependencies.

## Quickstart

```sh
cp cronkite.conf.example cronkite.conf
```

```yaml
version: 1

tasks:
  - name: logs
    type: logclean
    roots: [/var/log/myapp]
    older_than: 30d
    protect: ["*.journal", "wtmp"]

  - name: temp
    type: tempclean
    roots: [/tmp, /var/tmp]
    older_than: 7d
```

```sh
cronkite check     # validate the configuration and its preconditions
cronkite plan      # what would happen, touching nothing
cronkite run --apply
cronkite report    # read the last run
```

Schedule it. cron on Linux:

```cron
30 3 * * * /usr/local/bin/cronkite run --apply >> /var/log/cronkite/cron.log 2>&1
```

Task Scheduler on Windows:

```powershell
schtasks /create /tn cronkite /tr "cronkite.exe run --apply" /sc daily /st 03:30
```

## Configuration

The full reference, with defaults and comments, is
[cronkite.conf.example](cronkite.conf.example). The rules that matter:

- **Unknown keys are errors.** `older_then` will not silently fall back to a
  default.
- **Every problem is reported at once**, not one per run.
- **Paths are expanded** (`$VAR`, `${VAR}`, `%VAR%`, leading `~`), so one file
  works on Linux, macOS and Windows.
- **Secrets never live in the file.** A token is read from the environment
  variable the config names.

Durations accept `d` and `w` in addition to Go's units: `older_than: 30d`.
Sizes accept binary and decimal suffixes: `max_delete_bytes: 5GiB`.

## Task types

| Type | Default retention | Default depth | Purpose |
| --- | --- | --- | --- |
| `logclean` | 30d | 4 | Rotated logs and application output |
| `tempclean` | 7d | 8 | `/tmp`, `/var/tmp`, `%TEMP%` |
| `backup` | 7 daily + 4 weekly snapshots | — | Incremental, verified hardlink snapshots of one tree |

The two cleanup types share one implementation with two presets: the risk
profile of deleting a log differs from deleting a temp file, the mechanics must
not. A `backup` task has its own mechanics and its own ceiling
(`max_backup_bytes`); what it does and does not do is described in
[docs/architecture.md](docs/architecture.md).

## The report

One JSON document per run, written to `<state_dir>/reports/run-<id>.json` and
mirrored to `latest.json` atomically.

```json
{
  "schema": 1,
  "run_id": "20261102T102739Z-16fc51c9",
  "mode": "apply",
  "status": "partial",
  "exit_code": 2,
  "totals": { "files_removed": 412, "bytes_freed": 40265318, "errors": 1, "skipped": 6 },
  "tasks": [
    {
      "name": "logs",
      "type": "logclean",
      "status": "partial",
      "planned": 413,
      "actions": [
        { "path": "/var/log/myapp/app.log.3.gz", "action": "delete", "size": 20480, "reason": "older_than_30d" }
      ],
      "errors": [
        { "path": "/var/log/myapp/locked.log", "error": "permission denied" }
      ]
    }
  ]
}
```

Counters are always complete. The per-path action list is capped by
`report.max_actions_per_task` and says so with `actions_truncated`.

## Exit codes

| Code | Meaning | What to do in cron |
| --- | --- | --- |
| `0` | Every selected task completed | nothing |
| `1` | Usage or configuration error; cronkite did not run | page a human |
| `2` | At least one task reported failures | alert, the rest still ran |
| `3` | Another run holds the lock | ignore |
| `4` | A safety ceiling refused the run; nothing was deleted | page a human |

Details and rationale: [docs/exit-codes.md](docs/exit-codes.md).

## Extending with your own scripts

The roadmap includes a `script` task type, so a site-specific maintenance step
can join a cronkite run instead of living in a separate cron entry with its own
logging, locking and alerting:

```yaml
tasks:
  - name: rotate
    type: script
    interpreter: bash
    file: /usr/local/share/cronkite/rotate-app-logs.sh
    timeout: 5m
```

The contract (environment, exit codes, result reporting) is documented in
[docs/architecture.md](docs/architecture.md).

## Status

v0.1.0, the engine and its contract: configuration, safety confinement, cleanup
tasks, locking, reports, CLI.

| Release | Scope |
| --- | --- |
| v0.1.0 | Engine, `logclean`, `tempclean`, lock, safety ceilings, reports, CI |
| v0.2.0 | `backup` (hardlink snapshots, manifest, verification), `script` task type, Telegram alerts, install and scheduler scripts |
| v0.3.0 | SMTP alerts, Windows parity verification, scheduling guide, containerized demo |

The roadmap is a sequence of tagged, working releases, not a wishlist: each one
is tagged only when its scope is complete and its CI is green.

## License

MIT. See [LICENSE](LICENSE).
