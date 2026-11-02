# Scheduling

cronkite does nothing on its own. This is how it gets attached to a clock, and
what to think about before you do.

## Before anything runs

Three commands, in this order, every time you change the configuration:

```sh
cronkite check  -config /etc/cronkite/cronkite.conf
cronkite plan   -config /etc/cronkite/cronkite.conf
cronkite run    -config /etc/cronkite/cronkite.conf --apply
```

`check` catches a broken file, a missing root and a wrong permission before a
scheduled run finds them at 03:30. `plan` shows what the run would do, including
what a script task would say about itself. Only then schedule it.

## Use the wrapper, not the binary

`scripts/cronkite-run.sh` (and `cronkite-run.ps1` on Windows) exist for one
reason: exit code `3` means *another run holds the lock*, which is normal and
must not page anyone. The wrapper turns `3` into success and passes every other
code through unchanged, so a real failure still fails the job.

Installing a job that calls the binary directly means every overlap looks like an
incident.

### cron

```cron
30 3 * * * /usr/local/bin/cronkite-run.sh >> /var/log/cronkite.log 2>&1
```

cron mails any non-zero exit. With the wrapper, that mail only happens for codes
worth reading.

### systemd

`/etc/systemd/system/cronkite.service`:

```ini
[Unit]
Description=cronkite maintenance run
Documentation=file:/usr/local/share/cronkite/docs/exit-codes.md

[Service]
Type=oneshot
ExecStart=/usr/local/bin/cronkite-run.sh
# A maintenance run is not a daemon: a failure is reported, never retried.
Restart=no
```

`/etc/systemd/system/cronkite.timer`:

```ini
[Unit]
Description=Run cronkite nightly

[Timer]
OnCalendar=*-*-* 03:30:00
# A missed run because the machine was off should happen once at boot, not be
# skipped until tomorrow.
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
```

```sh
systemctl enable --now cronkite.timer
```

`RandomizedDelaySec` matters on a fleet: without it, every host hits its
destination at the same second.

### Windows Task Scheduler

```powershell
schtasks /create /tn cronkite /tr "powershell -NoProfile -File C:\Program Files\cronkite\cronkite-run.ps1" /sc daily /st 03:30
```

`scripts/install.ps1 -Register` does the same thing after installing.

Task Scheduler records any non-zero exit as a failed task. The wrapper makes a
locked run exit `0`, so the task's history stays readable.

## What a scheduled run should be allowed to do

- **Run as the user that owns the data.** A maintenance run as `root` will happily
  delete files a service needed; a run as the wrong user will silently fail on
  permissions. cronkite reports "removed nothing because of permissions" as a
  failure, so you will see it.
- **Keep the report.** Every run writes `run-<id>.json` and `latest.json` under
  the state directory. That is the record of what happened; back it up or ship it
  somewhere if the machine is disposable.
- **Alert on outcomes, not on activity.** `notify.on: [failure, partial]` is the
  setting that stays useful for years. `always` is for a week of tuning.
- **Do not run two schedules for the same task.** The lock prevents overlap, but
  the second run's exit code `3` is noise you created yourself.

## Verifying a schedule actually fires

```sh
# Ask the operating system, not your memory.
systemctl list-timers cronkite.timer
crontab -l | grep cronkite
schtasks /query /tn cronkite /v /fo LIST

# Then ask cronkite what it did.
cronkite report -json | jq '.run_id, .status, .totals'
```
