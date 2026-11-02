#!/usr/bin/env bash
#
# scripts/cronkite-run.sh — run cronkite from cron, systemd or a container.
#
# Why a wrapper instead of calling cronkite directly: exit code 3 means "another
# run holds the lock", which is normal on a busy box and must not wake anyone up.
# Every other non-zero code is a real signal and is passed through unchanged, so
# cron's mail and systemd's failure state keep working.
#
#   */30 * * * * /usr/local/lib/cronkite/cronkite-run.sh >> /var/log/cronkite/cron.log 2>&1
#
# Configuration is read from the standard locations, or from CRONKITE_CONFIG.

set -uo pipefail

# The wrapper ships next to the binary it runs, so an install under any prefix
# works without the operator having to export anything. CRONKITE_BIN still wins
# when set, for the case where the two are deliberately separated.
wrapper_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cronkite_bin=${CRONKITE_BIN:-${wrapper_dir}/cronkite}

# Built as one non-empty array. macOS still ships bash 3.2, where expanding an
# empty array under `set -u` is an unbound-variable error: a wrapper that dies
# before running the tool is worse than no wrapper.
run_args=(run --apply)
if [ -n "${CRONKITE_CONFIG:-}" ]; then
	run_args+=(-config "$CRONKITE_CONFIG")
fi
run_args+=("$@")

if [ ! -x "$cronkite_bin" ]; then
	echo "cronkite-run: ${cronkite_bin} is not executable; set CRONKITE_BIN or install cronkite first" >&2
	exit 1
fi

"$cronkite_bin" "${run_args[@]}"
code=$?

case $code in
0)
	# Every task completed.
	exit 0
	;;
3)
	# Another run holds the lock. Not a failure, and not worth an alert: a busy
	# box has overlapping runs and waking someone for it teaches them to ignore
	# the channel.
	echo "cronkite-run: previous run still in progress; nothing to do"
	exit 0
	;;
*)
	# 1 usage, 2 partial, 4 refused: all worth a human.
	exit "$code"
	;;
esac
