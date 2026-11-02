# Exit codes

These values are part of cronkite's public contract. Automation depends on them,
so changing one is a breaking change and requires a major release.

| Code | Name | Meaning |
| --- | --- | --- |
| `0` | ok | Every selected task completed with no failures. |
| `1` | error | Usage mistake, unreadable or invalid configuration, or an internal fault. cronkite did not run. |
| `2` | partial | cronkite ran and at least one task reported failures. Other tasks still ran. |
| `3` | locked | Another run holds the lock. Nothing was done, and that is normal. |
| `4` | refused | A safety ceiling stopped the run **before** anything was deleted. |

## Why these five and not "0 or 1"

A scheduled maintenance job has four genuinely different outcomes, and an
operator needs to react differently to each one:

- `3` must **not** alert. Overlapping with a slow previous run is expected. If
  it alerts, the alert channel loses credibility.
- `4` must always alert, and it means a human must look at the configuration.
  It is the code that exists to prevent an accident.
- `2` means the run did most of its job; the failures are worth a look but the
  machine is not in the state it was before.
- `1` means nothing happened at all, which is a different kind of broken.

Collapsing them into success/failure throws away exactly the information cron
needs to route the alert.

## Using them

Linux, with cron and a simple wrapper:

```cron
30 3 * * * /usr/local/bin/cronkite run --apply >> /var/log/cronkite/cron.log 2>&1
```

`cron` mails any non-zero exit, including `3`. To avoid waking up for lock
contention, gate on the code:

```sh
#!/bin/sh
/usr/local/bin/cronkite run --apply -config /etc/cronkite/cronkite.conf
code=$?
case $code in
  0|3) exit 0 ;;
  *)   exit $code ;;
esac
```

systemd:

```ini
[Service]
Type=oneshot
ExecStart=/usr/local/bin/cronkite run --apply
# Exit 2 (partial) is still recorded by systemd, and a "Restart=" is not used
# because a maintenance run is not a daemon.
SuccessExitStatus=0
```

Windows Task Scheduler treats any non-zero exit as a failure and records
`LastTaskResult`. That is the desired behaviour for `1`, `2` and `4`; if exit `3`
appears regularly, the schedule is too dense for the workload.

## The report carries the same information

`exit_code` is written into the JSON report next to `status`, so a monitoring
agent that consumes reports can alert on history, not only on the last exit:

```sh
cronkite report -json | jq '.status, .totals.errors'
```
