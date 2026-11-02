package report

import "testing"

// The bucket an action lands in is the difference between a report that says
// "freed 1 MiB" and one that says "wrote 1 MiB". Confusing them describes a
// backup as a deletion.
func TestWithActionBuckets(t *testing.T) {
	tests := []struct {
		name string
		kind ActionKind
		size int64
		want Totals
	}{
		{"delete frees", ActionDelete, 100, Totals{FilesRemoved: 1, BytesFreed: 100}},
		{"rmdir counts a directory", ActionRmdir, 0, Totals{DirsRemoved: 1}},
		{"write writes", ActionWrite, 250, Totals{FilesWritten: 1, BytesWritten: 250}},
		{"relink reuses", ActionRelink, 400, Totals{FilesRelinked: 1, BytesRelinked: 400}},
		// An unknown kind must never be counted as destroyed data.
		{"unknown is work, not loss", ActionKind("invented-later"), 7, Totals{FilesWritten: 1, BytesWritten: 7}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Totals{}.WithAction(tc.kind, tc.size)
			if got != tc.want {
				t.Fatalf("WithAction(%q, %d) = %+v, want %+v", tc.kind, tc.size, got, tc.want)
			}
			if got.FilesRemoved != 0 && tc.kind != ActionDelete {
				t.Fatalf("%q was counted as a removal", tc.kind)
			}
		})
	}
}

func TestAddSumsEveryBucket(t *testing.T) {
	a := Totals{FilesRemoved: 1, BytesFreed: 10, FilesWritten: 2, BytesWritten: 20, FilesRelinked: 3, BytesRelinked: 30, Errors: 4, Skipped: 5}
	b := Totals{FilesRemoved: 1, BytesFreed: 1, FilesWritten: 1, BytesWritten: 2, FilesRelinked: 1, BytesRelinked: 3, Errors: 1, Skipped: 1}

	got := a.Add(b)
	want := Totals{FilesRemoved: 2, BytesFreed: 11, FilesWritten: 3, BytesWritten: 22, FilesRelinked: 4, BytesRelinked: 33, Errors: 5, Skipped: 6}
	if got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
}
