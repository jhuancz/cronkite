package notify

import (
	"strings"
	"testing"

	"github.com/jhuancz/cronkite/internal/report"
)

// The event filter is the difference between an alert channel an operator reads
// and one they mute, so every configured event is pinned against every status.
func TestFilterAllows(t *testing.T) {
	tests := []struct {
		name   string
		on     []string
		status report.Status
		want   bool
	}{
		{"never, ok", []string{"never"}, report.StatusOK, false},
		{"never, failed", []string{"never"}, report.StatusFailed, false},
		{"never wins over another event", []string{"never", "failure"}, report.StatusFailed, false},
		{"failure, ok", []string{"failure"}, report.StatusOK, false},
		{"failure, skipped", []string{"failure"}, report.StatusSkipped, false},
		{"failure, partial", []string{"failure"}, report.StatusPartial, true},
		{"failure, failed", []string{"failure"}, report.StatusFailed, true},
		{"failure, refused", []string{"failure"}, report.StatusRefused, true},
		{"partial, ok", []string{"partial"}, report.StatusOK, false},
		{"partial, partial", []string{"partial"}, report.StatusPartial, true},
		{"partial, failed", []string{"partial"}, report.StatusFailed, true},
		{"default list, ok", []string{"failure", "partial"}, report.StatusOK, false},
		{"default list, partial", []string{"failure", "partial"}, report.StatusPartial, true},
		{"always, ok", []string{"always"}, report.StatusOK, true},
		{"always, skipped", []string{"always"}, report.StatusSkipped, true},
		{"always, refused", []string{"always"}, report.StatusRefused, true},
		{"empty sends nothing", nil, report.StatusFailed, false},
		{"unknown sends nothing", []string{"whenever"}, report.StatusFailed, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewFilter(tc.on).Allows(tc.status); got != tc.want {
				t.Fatalf("NewFilter(%v).Allows(%q) = %v, want %v", tc.on, tc.status, got, tc.want)
			}
		})
	}
}

// The order is part of the throttle's contract, not an implementation detail:
// it is how "a worse outcome is never suppressed" is decided.
func TestSeverityOrder(t *testing.T) {
	if !(SeverityOK < SeverityPartial && SeverityPartial < SeverityFailed) {
		t.Fatalf("severity must order ok < partial < failed, got %d %d %d",
			SeverityOK, SeverityPartial, SeverityFailed)
	}
	cases := map[report.Status]Severity{
		report.StatusOK:      SeverityOK,
		report.StatusSkipped: SeverityOK,
		report.StatusPartial: SeverityPartial,
		report.StatusFailed:  SeverityFailed,
		report.StatusRefused: SeverityFailed,
	}
	for status, want := range cases {
		if got := SeverityOf(status); got != want {
			t.Errorf("SeverityOf(%q) = %d, want %d", status, got, want)
		}
	}
}

// One message per run has to stand on its own: an operator reads it on a phone,
// away from the report.
func TestMessageTextCarriesWhatAnOperatorNeeds(t *testing.T) {
	m := Message{
		Status:      report.StatusPartial,
		RunID:       "20260301T120000Z-abcd1234",
		Host:        "web-01",
		Totals:      report.Totals{FilesRemoved: 3, BytesFreed: 2048, Skipped: 2, Errors: 1},
		FailedTasks: []string{"logs", "temp"},
		ReportPath:  "/var/lib/cronkite/reports/run-20260301T120000Z-abcd1234.json",
	}
	text := m.Text()
	for _, want := range []string{
		"partial",
		"20260301T120000Z-abcd1234",
		"web-01",
		"removed: 3 files, 2.0 KiB",
		"skipped: 2, errors: 1",
		"logs, temp",
		"/var/lib/cronkite/reports/run-20260301T120000Z-abcd1234.json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the message should contain %q, got:\n%s", want, text)
		}
	}
}
