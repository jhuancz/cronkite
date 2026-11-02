// Package version carries build identity, injected at link time by the
// Makefile and the release pipeline.
package version

import "runtime"

// Set with -ldflags at build time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String is the short identity used in reports and log headers.
func String() string {
	return Version + " (" + Commit + ")"
}

// UserAgent identifies cronkite to remote APIs.
func UserAgent() string {
	return "cronkite/" + Version + " (" + runtime.GOOS + " " + runtime.GOARCH + ")"
}
