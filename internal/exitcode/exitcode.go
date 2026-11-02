// Package exitcode is the documented contract between cronkite and whatever
// runs it: cron, Task Scheduler, systemd, or a monitoring agent. These values
// are API. Changing one is a breaking change.
package exitcode

const (
	// OK means every selected task completed with no failures.
	OK = 0

	// Error covers usage mistakes, unreadable configuration, and internal
	// faults: cronkite could not run at all.
	Error = 1

	// Partial means cronkite ran, but at least one task reported failures.
	// Scheduled runs should alert on this.
	Partial = 2

	// Locked means another run holds the lock. Not a failure: cron overlapped
	// with a slow previous run.
	Locked = 3

	// Refused means a safety limit stopped the run before anything was
	// deleted. This is the one code that always deserves a human.
	Refused = 4
)
