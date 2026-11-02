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
		verb = plannedVerb(r)
	}

	fmt.Fprintf(w, "cronkite %s  run %s  %s  status=%s\n", r.Tool.Version, r.RunID, r.Mode, r.Status)
	fmt.Fprintf(w, "host %s (%s/%s)  config %s\n\n", r.Host.Hostname, r.Host.OS, r.Host.Arch, r.ConfigPath)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  TASK\tTYPE\tSTATUS\tPLANNED\t%s\tWRITTEN\tSIZE\tERRORS\n", strings.ToUpper(verb))
	for _, task := range r.Tasks {
		count := zeroDash(task.Totals.FilesRemoved)
		written := zeroDash(task.Totals.FilesWritten + task.Totals.FilesRelinked)
		size := task.Totals.BytesFreed + task.Totals.BytesWritten + task.Totals.BytesRelinked
		if r.Mode == ModePlan {
			// Only a per-file plan can show per-file counts. A backup plans
			// aggregate work, so its row reports the size and says the rest in its
			// notes instead of pretending an action count is a file count.
			count = zeroDash(countKind(task.Actions, ActionDelete, ActionRmdir))
			written = "-"
			size = task.PlannedBytes
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			task.Name, task.Type, task.Status, task.Planned, count, written,
			bytesx.Format(size), zeroDash(task.Totals.Errors),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Each figure names its own unit. A run that only wrote files must not be
	// summarised as having removed anything, and a backup that shares data with an
	// earlier snapshot says how much of its work was a link rather than a copy.
	parts := make([]string, 0, 3)
	if r.Mode == ModePlan {
		removals := countKind(allActions(r), ActionDelete, ActionRmdir)
		writes := countKind(allActions(r), ActionWrite, ActionRelink)
		if removals > 0 {
			parts = append(parts, fmt.Sprintf("%s files, %s %s", zeroDash(removals), bytesx.Format(bytesKind(allActions(r), ActionDelete, ActionRmdir)), verb))
		}
		if writes > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", bytesx.Format(bytesKind(allActions(r), ActionWrite, ActionRelink)), verb))
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing to do")
		}
	} else {
		if r.Totals.FilesRemoved > 0 {
			parts = append(parts, fmt.Sprintf("%d files, %s removed", r.Totals.FilesRemoved, bytesx.Format(r.Totals.BytesFreed)))
		}
		if written := r.Totals.FilesWritten + r.Totals.FilesRelinked; written > 0 {
			detail := ""
			if r.Totals.FilesRelinked > 0 {
				detail = fmt.Sprintf(", %d reusing earlier data", r.Totals.FilesRelinked)
			}
			parts = append(parts, fmt.Sprintf("%d files written (%s%s)",
				written, bytesx.Format(r.Totals.BytesWritten+r.Totals.BytesRelinked), detail))
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing to do")
		}
	}
	parts = append(parts, fmt.Sprintf("%s skipped, %s errors", zeroDash(r.Totals.Skipped), zeroDash(r.Totals.Errors)))
	fmt.Fprintf(w, "\n  total: %s\n", strings.Join(parts, ", "))

	if failures := failures(r); len(failures) > 0 {
		fmt.Fprintf(w, "\n  failures:\n%s", indent(failures))
	}
	if skipped := skippedReasons(r); len(skipped) > 0 {
		fmt.Fprintf(w, "\n  skipped:\n%s", indent(skipped))
	}
	for _, task := range r.Tasks {
		if len(task.Notes) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s:\n", task.Name)
		fmt.Fprint(w, indent(task.Notes))
	}
	if len(r.Notes) > 0 {
		fmt.Fprintf(w, "\n  notes:\n%s", indent(r.Notes))
	}

	if r.Mode == ModePlan && r.Totals.Errors == 0 && r.Planned() > 0 {
		fmt.Fprintf(w, "\n  nothing was changed. re-run with --apply to execute.\n")
	}
	return nil
}

// plannedVerb names what a rehearsal would actually do, so a backup plan is not
// described as a deletion.
func plannedVerb(r *Report) string {
	removals := countKind(allActions(r), ActionDelete, ActionRmdir)
	writes := countKind(allActions(r), ActionWrite, ActionRelink)
	switch {
	case writes > 0 && removals > 0:
		return "would copy or remove"
	case writes > 0:
		return "would write"
	case removals > 0:
		return "would remove"
	default:
		// Nothing was planned, usually because planning failed; naming a verb
		// here would describe work that does not exist.
		return "would do"
	}
}

// allActions flattens the planned or performed actions of every task.
func allActions(r *Report) []Action {
	var out []Action
	for _, t := range r.Tasks {
		out = append(out, t.Actions...)
	}
	return out
}

func countKind(actions []Action, kinds ...ActionKind) int {
	n := 0
	for _, a := range actions {
		for _, k := range kinds {
			if a.Action == k {
				n++
				break
			}
		}
	}
	return n
}

func bytesKind(actions []Action, kinds ...ActionKind) int64 {
	var total int64
	for _, a := range actions {
		for _, k := range kinds {
			if a.Action == k {
				total += a.Size
				break
			}
		}
	}
	return total
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
		return r.Totals.BytesFreed + r.Totals.BytesWritten + r.Totals.BytesRelinked
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
