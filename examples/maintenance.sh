#!/usr/bin/env bash
#
# examples/maintenance.sh — a site maintenance script driven by cronkite.
#
# cronkite runs this file as a `script` task, which means it inherits the run's
# lock, its report and its rehearsal mode. The contract it receives:
#
#   CRONKITE_RUN_ID   the id of the report this run writes; log it and you can
#                     match your own lines against that report
#   CRONKITE_MODE     plan or apply
#   CRONKITE_DRY_RUN  1 during a rehearsal, 0 when the run may change things
#   CRONKITE_TASK     the task name from the configuration
#
# Two rules make a script safe to schedule:
#   - honour CRONKITE_DRY_RUN; a rehearsal that deletes something is a bug
#   - exit non-zero when something went wrong; cronkite records the status and
#     keeps going with the next task, which is better than dying quietly
#
# Try it:  CRONKITE_DRY_RUN=1 LOG_DIR=/tmp/demo ./examples/maintenance.sh

set -euo pipefail

log_dir=${LOG_DIR:-/var/log/myapp}
keep_days=${KEEP_DAYS:-14}
dry_run=${CRONKITE_DRY_RUN:-1}

echo "${CRONKITE_TASK:-maintenance}: run=${CRONKITE_RUN_ID:-none} mode=${CRONKITE_MODE:-manual} dry_run=${dry_run}"
echo "scanning ${log_dir} for *.log older than ${keep_days} days"

if [ ! -d "$log_dir" ]; then
	echo "nothing to do: ${log_dir} does not exist"
	exit 0
fi

count=0
# -print0 with read -d '': a filename with a space or a newline cannot be split
# into two paths, and "$file" is always one argument to rm.
while IFS= read -r -d '' file; do
	count=$((count + 1))
	if [ "$dry_run" = "1" ]; then
		echo "would remove ${file}"
	else
		rm -f -- "$file"
		echo "removed ${file}"
	fi
done < <(find "$log_dir" -type f -name '*.log' -mtime +"$keep_days" -print0)

echo "${count} file(s) matched"
