# Safety model

cronkite deletes files on production servers. Everything here exists because a
plausible mistake in one of these areas is unrecoverable.

The design rule: **a safety property must be enforced by the machine, not by a
comment**. Where the kernel can enforce it, the kernel does.

## 1. Nothing changes unless asked

`plan` and `run` without `--apply` are rehearsals. They read the filesystem,
build a plan, write a report, and change nothing.

This is not a separate code path that can drift from the real one. A task
implements two phases:

```go
Plan(ctx, env)  (*Plan, error)   // what would happen
Apply(ctx, plan, env) (*Result, error)  // do it
```

A rehearsal is the same `Plan` with `Apply` skipped. If a rehearsal and a real
run ever disagree, that is a bug in one function, not in two.

## 2. Deletion cannot leave a configured root

`roots` is a hard boundary. Each root is opened with `os.OpenRoot`, and every
removal goes through that handle with a root-relative name.

Go's `os.Root` resolves paths inside its tree and refuses to traverse a symlink
that escapes it. That means:

- `..` cannot climb out;
- a symlink at `<root>/logs -> /etc` cannot be used to reach `/etc`;
- the boundary does not depend on cronkite's own string handling being correct.

On top of that, `internal/fsx` refuses to remove the configured root itself, and
resolves symlinked *ancestors* of the addressed path (macOS `/var` is
`/private/var`, containers do the same with `/tmp`) without ever resolving the
final component. That distinction matters:

```
/var/log/app/old.log       -> /private/var/log/app/old.log   fine
/var/log/app/current  ->  /etc/passwd                          removes the LINK
```

Removing a link removes the link. It never removes what the link points at.

## 3. A typo cannot wipe a filesystem

Two ceilings, applied to the **whole run**, checked **before the first
deletion**:

```yaml
run:
  safety:
    max_delete_files: 500
    max_delete_bytes: 5GiB
```

Exceeding either one refuses the entire run with exit code `4` and a message
that says what the plan was and what the limit is. Nothing is partially done:
the engine plans every task first, then enforces the ceilings, then applies.

`--force` bypasses the ceilings. The bypass is written into the report, so a
later reader can tell a legitimate bulk cleanup from an accident that nobody
noticed.

## 4. Symlinks and special files are never removed

The walk does not descend into symlinks, and symlinked leaves are recorded as
skipped (`reason: symlink`) instead of deleted. Devices, sockets and FIFOs are
recorded as `not_a_regular_file` for the same reason: cronkite cannot prove what
they are, and the cost of keeping a stale link is lower than the cost of being
wrong.

## 5. Runs cannot overlap

`run --apply` takes a non-blocking exclusive lock at `run.lock_file`. If another
run holds it, cronkite exits `3` immediately and touches nothing.

Two deletes racing on the same tree produce the worst kind of report: a thousand
"file not found" failures that describe a bug which does not exist. The lock
makes that state unrepresentable, and exit code `3` tells cron this was not a
failure.

## 6. Silence is impossible

Every candidate ends up in exactly one bucket, and the report carries all of
them:

- **removed** — action `delete`, with size and reason (`older_than_30d`);
- **skipped** — with a reason: `protected`, `symlink`, `locked_or_forbidden`,
  `not_a_regular_file`;
- **failed** — with the error.

There is no "clean up quietly" mode.

## 7. A broken privilege setup is loud

Locked and forbidden files are *skipped*, not failed: a log held open by another
service is an expected condition on a live machine, and alerting on it trains
operators to ignore alerts.

But if a task removed **nothing** and the only reason was permissions, that is a
configuration or privilege problem, not noise. cronkite reports it as a failure
with a message naming the cause, so a misconfigured service account cannot pass
as a successful run.

## 8. Protection is explicit and inherited

```yaml
protect:
  - "*.journal"     # any file named like this, at any depth
  - "cronkite/*"    # any path ending in cronkite/...
  - "wtmp"
```

Patterns without a separator match the file name. Patterns with a separator
match the full path or any trailing part of it. Protecting a directory protects
its whole subtree, and protection is re-checked at removal time, so a path that
became protected between planning and applying is still safe.

## 9. Secrets stay out of the configuration

Configuration files get copied into tickets and pasted into chat. The alert
token is read from the environment variable that `notify.telegram.token_env`
names, and the report never contains it.

## What is explicitly out of scope

- cronkite does not report the contents of the files it deletes.
- cronkite does not restore anything. Backup restore verification arrives with
  the `backup` task in v0.2.0.
- cronkite is not a security scanner. A protected pattern is an operational
  guard, not an audit control.
