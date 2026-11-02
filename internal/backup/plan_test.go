package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A rehearsal must leave nothing behind: no destination directory, no staging
// tree, no snapshot. A plan is a reading of the source and of the newest
// snapshot, never a write.
func TestPlanCreatesNothing(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	dest := filepath.Join(t.TempDir(), "backups")

	plan, err := newWriter(t, []string{src}, dest).Plan(context.Background(), firstRun.In(time.FixedZone("UTC+2", 7200)))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Named in UTC, like the snapshot the run would publish.
	if want := filepath.Join(dest, "20260301T100000Z"); plan.Snapshot != want {
		t.Fatalf("planned snapshot = %q, want %q", plan.Snapshot, want)
	}
	if plan.Previous != "" {
		t.Fatalf("previous = %q, want none before the first snapshot", plan.Previous)
	}
	if plan.Copied != 2 || plan.Relinked != 0 {
		t.Fatalf("copied = %d, relinked = %d, want 2 and 0", plan.Copied, plan.Relinked)
	}
	if want := int64(len("alpha") + len("bravo!")); plan.BytesCopied != want {
		t.Fatalf("bytes copied = %d, want %d", plan.BytesCopied, want)
	}
	if plan.BytesRelinked != 0 {
		t.Fatalf("bytes deduplicated = %d, want 0", plan.BytesRelinked)
	}
	if plan.Files() != 2 {
		t.Fatalf("the planned snapshot would hold %d files, want 2", plan.Files())
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plan mode must not create the destination, stat err = %v", err)
	}
}

// The plan and the run that follows it must agree file for file and byte for
// byte: the figures come from the same decision, so a rehearsal cannot promise a
// copy the run relinks, nor miss one it copies.
func TestPlanMatchesTheRunItPlans(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	writeSourceFile(t, src, "logs/b.log", "bravo!")
	dest := filepath.Join(t.TempDir(), "backups")
	w := newWriter(t, []string{src}, dest)
	ctx := context.Background()

	first, err := w.Plan(ctx, firstRun)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	run, err := w.Write(ctx, firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertPlanMatchesRun(t, first, run)

	// A second run after one file changed has to plan one copy and one relink:
	// the unchanged file is reused, the edited one is written again.
	changed := writeSourceFile(t, src, "logs/b.log", "bravo, and again!")
	written := firstRun.Add(time.Hour)
	if err := os.Chtimes(changed, written, written); err != nil {
		t.Fatal(err)
	}
	later := firstRun.Add(24 * time.Hour)

	second, err := w.Plan(ctx, later)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if second.Previous != run.Snapshot {
		t.Fatalf("previous = %q, want the snapshot the first run published (%q)", second.Previous, run.Snapshot)
	}
	if second.Copied != 1 || second.Relinked != 1 {
		t.Fatalf("planned copied = %d, relinked = %d, want 1 and 1", second.Copied, second.Relinked)
	}
	run, err = w.Write(ctx, later)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	assertPlanMatchesRun(t, second, run)
}

// assertPlanMatchesRun compares a plan with the run it planned. A fallback would
// copy bytes the plan counted as reused, and a plan cannot know that in advance,
// so the comparison only holds when no hardlink failed.
func assertPlanMatchesRun(t *testing.T, plan *Plan, run *Result) {
	t.Helper()
	if len(run.Fallbacks) != 0 {
		t.Fatalf("unexpected relink fallbacks: %+v", run.Fallbacks)
	}
	if plan.Snapshot != run.Snapshot {
		t.Errorf("planned snapshot = %q, run published %q", plan.Snapshot, run.Snapshot)
	}
	if plan.Copied != run.Copied || plan.Relinked != run.Relinked {
		t.Errorf("plan copied/relinked = %d/%d, run = %d/%d", plan.Copied, plan.Relinked, run.Copied, run.Relinked)
	}
	if plan.BytesCopied != run.BytesCopied || plan.BytesRelinked != run.BytesRelinked {
		t.Errorf("plan bytes copied/reused = %d/%d, run = %d/%d",
			plan.BytesCopied, plan.BytesRelinked, run.BytesCopied, run.BytesRelinked)
	}
}

// What a plan leaves out is exactly what the run leaves out, because both ask the
// same question: a skipped file is not a copy the plan forgot to mention.
func TestPlanReportsTheSameSkipsAsTheRun(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.txt", "alpha")
	// The snapshot's own index file cannot be a source entry.
	writeSourceFile(t, src, ManifestName, `{"files":[]}`)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("not a source file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ctx := context.Background()
	w := newWriter(t, []string{src}, filepath.Join(t.TempDir(), "backups"))
	plan, err := w.Plan(ctx, firstRun)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	run, err := w.Write(ctx, firstRun)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := map[string]string{
		ManifestName: ReasonReservedName,
		"link.txt":   ReasonSymlink,
	}
	if len(plan.Skipped) != len(want) || len(run.Skipped) != len(want) {
		t.Fatalf("plan skipped %+v, run skipped %+v, want %d entries each", plan.Skipped, run.Skipped, len(want))
	}
	for _, lists := range [][]Skip{plan.Skipped, run.Skipped} {
		for _, s := range lists {
			if reason, ok := want[s.Path]; !ok || reason != s.Reason {
				t.Errorf("skipped %q with reason %q, want one of %v", s.Path, s.Reason, want)
			}
		}
	}
	if plan.Files() != 1 || run.Copied != 1 {
		t.Errorf("plan counted %d files, run copied %d, want one of each", plan.Files(), run.Copied)
	}
}
