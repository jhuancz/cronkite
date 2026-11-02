package main

import (
	"os"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/jhuancz/cronkite/internal/durationx"
)

// TestMain wires the real CLI into testscript, so every scenario below drives
// the same code path an operator runs, in a real temporary filesystem, with
// real exit codes.
func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(m, map[string]func() int{
		"cronkite": func() int { return run(os.Args[1:], os.Stdout, os.Stderr) },
	}))
}

func TestScript(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"touchold": touchOld,
		},
	})
}

// touchOld backdates a file so age-based rules can be exercised without waiting
// thirty days. Usage: touchold <path> <age>
func touchOld(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("touchold does not support the ! prefix")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: touchold <path> <age>")
	}
	age, err := durationx.Parse(args[1])
	if err != nil {
		ts.Fatalf("touchold: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(ts.MkAbs(args[0]), when, when); err != nil {
		ts.Fatalf("touchold: %v", err)
	}
}
