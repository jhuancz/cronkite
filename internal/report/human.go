package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jhuancz/cronkite/internal/bytesx"
)

// Human writes the operator-facing summary printed in a terminal. The JSON
// report stays the source of truth; this is the readable mirror of it.
func Human(w io.Writer, r *Report) error {
	verb := "removed"
	if r.Mode == ModePlan {
		verb = "would remove"
	}

	fmt.Fprintf(w, "cronkite %s  run %s  %s  status=%s\n", r.Tool.Version, r.RunID, r.Mode, r.Status)
	fmt.Fprintf(w, "host %s (%s/%s)  config %s\n\n", r.Host.Hostname, r.Host.OS, r.Host.Arch, r.ConfigPath)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  TASK\tTYPE\tSTATUS\tPLANNED\t%s\tSIZE\tERRORS\n", strings.ToUpper(verb))
	for _, task := range r.Tasks {
		count := zeroDash(task.Totals.FilesRemoved)
		size := task.Totals.BytesFreed
		if r.Mode == ModePlan {
			count = fmt.Sprintf("%d", task.Planned)
			size = task.PlannedBytes
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			task.Name, task.Type, task.Status, task.Planned, count,
			bytesx.Format(size), zeroDash(task.Totals.Errors),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fileCount := r.Totals.FilesRemoved
	if r.Mode == ModePlan {
		fileCount = r.Planned()
	}
	fmt.Fprintf(w, "\n  total: %s files, %s %s, %s skipped, %s errors\n",
		zeroDash(fileCount),
		bytesx.Format(r.affectedBytes()), verb,
		zeroDash(r.Totals.Skipped),
		zeroDash(r.Totals.Errors),
	)

	if failures := failures(r); len(failures) > 0 {
		fmt.Fprintf(w, "\n  failures:\n%s", indent(failures))
	}
	if skipped := skippedReasons(r); len(skipped) > 0 {
		fmt.Fprintf(w, "\n  skipped:\n%s", indent(skipped))
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(w, "\n  notes:\n%s", indent(r.Notes))
	}

	if r.Mode == ModePlan && r.Totals.Errors == 0 && r.Planned() > 0 {
		fmt.Fprintf(w, "\n  nothing was changed. re-run with --apply to execute.\n")
	}
	return nil
}

// Planned is the number of actions the run intended, across every task.
func (r *Report) Planned() int {
	total := 0
	for _, task := range r.Tasks {
		total += task.Planned
	}
	return total
}

// affectedBytes is the byte count that matters for the mode: what was freed in
// apply mode, what would be freed in plan mode.
func (r *Report) affectedBytes() int64 {
	if r.Mode != ModePlan {
		return r.Totals.BytesFreed
	}
	var total int64
	for _, task := range r.Tasks {
		total += task.PlannedBytes
	}
	return total
}

func failures(r *Report) []string {
	var out []string
	for _, task := range r.Tasks {
		for _, f := range task.Errors {
			out = append(out, fmt.Sprintf("%s: %s: %s", task.Name, f.Path, f.Error))
		}
	}
	return out
}

// skippedReasons collapses per-path skip noise into counts per reason, so a
// thousand protected files do not bury the actionable lines.
func skippedReasons(r *Report) []string {
	counts := map[string]int{}
	var order []string
	for _, task := range r.Tasks {
		for _, a := range task.Actions {
			if a.Reason == "" || isRemoval(a) {
				continue
			}
			key := task.Name + ": " + a.Reason
			if _, seen := counts[key]; !seen {
				order = append(order, key)
			}
			counts[key]++
		}
	}
	var out []string
	for _, key := range order {
		out = append(out, fmt.Sprintf("%s (%d files)", key, counts[key]))
	}
	return out
}

func isRemoval(a Action) bool { return a.Action == ActionDelete || a.Action == ActionRmdir }

func indent(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("    - " + line + "\n")
	}
	return b.String()
}

func zeroDash(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}
