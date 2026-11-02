#!/usr/bin/env bash
#
# scripts/install.sh — install cronkite on a Linux or macOS host.
#
#   ./install.sh                 install ./cronkite (or build one) to /usr/local/bin
#   ./install.sh --prefix ~/.local
#   ./install.sh --dry-run       show what would happen
#
# It installs the binary, creates the configuration directory, copies the example
# configuration when none exists, and prints the cron line. It does not start
# scheduling for you: enabling a job that deletes files is your decision, not an
# installer's.

set -euo pipefail

prefix=/usr/local
bindir=
configdir=
binary=
dry_run=0
with_wrapper=1

usage() {
	cat <<'EOF'
usage: install.sh [options]

  --prefix DIR     install prefix (default: /usr/local)
  --bindir DIR     where the binary goes (default: PREFIX/bin)
  --configdir DIR  configuration directory (default: /etc/cronkite)
  --binary PATH    install this binary instead of ./cronkite or a local build
  --no-wrapper     do not install the cron wrapper
  --dry-run        print the actions without performing them
  -h, --help       this text
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--prefix)
		prefix=${2:?--prefix needs a value}
		shift 2
		;;
	--bindir)
		bindir=${2:?--bindir needs a value}
		shift 2
		;;
	--configdir)
		configdir=${2:?--configdir needs a value}
		shift 2
		;;
	--binary)
		binary=${2:?--binary needs a value}
		shift 2
		;;
	--no-wrapper)
		with_wrapper=0
		shift
		;;
	--dry-run)
		dry_run=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "install.sh: unknown option $1" >&2
		usage >&2
		exit 1
		;;
	esac
done

bindir=${bindir:-${prefix}/bin}
configdir=${configdir:-/etc/cronkite}

run() {
	if [ "$dry_run" = "1" ]; then
		printf 'would run: %s\n' "$*"
	else
		"$@"
	fi
}

say() { printf '%s\n' "$*"; }

# 1. Find a binary: an explicit one, one next to this script, or a local build.
if [ -z "$binary" ]; then
	repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
	for candidate in "${repo_root}/cronkite" "$(command -v cronkite || true)"; do
		if [ -n "$candidate" ] && [ -x "$candidate" ]; then
			binary=$candidate
			break
		fi
	done
fi
if [ -z "$binary" ] || [ ! -x "$binary" ]; then
	echo "install.sh: no cronkite binary found; run 'make build' or pass --binary PATH" >&2
	exit 1
fi

wrapper_path='not installed (--no-wrapper)'
if [ "$with_wrapper" = "1" ]; then
	wrapper_path="${bindir}/cronkite-run.sh"
fi

say "binary:  ${binary}"
say "install: ${bindir}/cronkite"
say "config:  ${configdir}/cronkite.conf"
say "wrapper: ${wrapper_path}"

# 2. Install the binary and the wrapper.
run install -d "$bindir"
run install -m 0755 "$binary" "${bindir}/cronkite"
if [ "$with_wrapper" = "1" ]; then
	repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
	run install -m 0755 "${repo_root}/scripts/cronkite-run.sh" "${bindir}/cronkite-run.sh"
fi

# 3. Configuration: never overwrite an existing file. An installer that replaces
#    a working retention policy with an example is an installer that deletes logs
#    on its next run.
run install -d -m 0755 "$configdir"
if [ -e "${configdir}/cronkite.conf" ]; then
	say "keeping existing ${configdir}/cronkite.conf"
else
	repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
	if [ -f "${repo_root}/cronkite.conf.example" ]; then
		run install -m 0644 "${repo_root}/cronkite.conf.example" "${configdir}/cronkite.conf"
		if [ "$dry_run" = "1" ]; then
			say "would write an example configuration to ${configdir}/cronkite.conf"
		else
			say "wrote an example configuration to ${configdir}/cronkite.conf; edit it before scheduling"
		fi
	fi
fi

# 4. Say what to do next, without doing it.
cat <<EOF

Installed. Next:

  1. Edit ${configdir}/cronkite.conf
  2. Check it:   ${bindir}/cronkite check -config ${configdir}/cronkite.conf
  3. Rehearse:   ${bindir}/cronkite plan -config ${configdir}/cronkite.conf
  4. Schedule it, using the wrapper so a locked run stays silent:

       30 3 * * * ${wrapper_path} >> /var/log/cronkite.log 2>&1

     The wrapper resolves the binary next to itself, so this line works for the
     prefix you chose. Point CRONKITE_CONFIG at ${configdir}/cronkite.conf if you
     installed the configuration somewhere cronkite does not search.

     or, with systemd:

       systemctl edit --force --full cronkite.service   # see docs/scheduling.md
       systemctl enable --now cronkite.timer

  Scheduling is deliberately manual: a job that deletes files should be enabled by
  the operator who configured it, not by an installer.
EOF
