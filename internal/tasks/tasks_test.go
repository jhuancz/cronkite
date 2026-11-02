package tasks

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhuancz/cronkite/internal/config"
)

// The backup type is wired into the same switch as the cleanup types, so one
// configuration file can run all of them.
func TestBuildConstructsABackupTask(t *testing.T) {
	cfg := config.Task{
		Name:        "data",
		Type:        config.TypeBackup,
		Roots:       []string{t.TempDir()},
		Destination: filepath.Join(t.TempDir(), "snapshots"),
	}

	runner, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })

	if runner.Name() != "data" {
		t.Errorf("Name() = %q, want %q", runner.Name(), "data")
	}
	if runner.Type() != config.TypeBackup {
		t.Errorf("Type() = %q, want %q", runner.Type(), config.TypeBackup)
	}
}

// Types is what `cronkite check` reports, so a supported type that is missing
// from it is invisible to the operator.
func TestTypesListsEverySupportedType(t *testing.T) {
	for _, want := range []string{config.TypeBackup, "logclean", "tempclean"} {
		found := false
		for _, have := range Types {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Types = %v, missing %q", Types, want)
		}
	}
}

func TestBuildRejectsUnknownType(t *testing.T) {
	_, err := Build(config.Task{Name: "x", Type: "archive"})
	if err == nil {
		t.Fatal("expected an unknown type to be refused")
	}
	if !strings.Contains(err.Error(), config.TypeBackup) {
		t.Fatalf("the error should list the supported types, got: %v", err)
	}
}
