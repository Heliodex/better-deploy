#!/bin/sh
# Rebuild the supervisor (including the embedded admin panel) and restart the local instance. A running supervisor's directories and binary path are reused, so deployed apps, their data and their logs survive the restart.
#
#   ./dev.sh                 # rebuild panel + binary, restart
#   ./dev.sh --skip-generate # Go-only change: skip the panel build
#
# Overrides: WT_DEV_ROOT, WT_DEV_BIN, WT_DEV_LOG, or the usual WT_APPS_DIR, WT_STATE_DIR, WT_LISTEN, WT_WASMTIME, WT_WASMTIME_TARGET and WT_TOKEN.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

generate=1
for arg in "$@"; do
	case $arg in
	--skip-generate | --no-panel) generate=0 ;;
	-h | --help)
		awk 'NR > 1 { if ($0 !~ /^#/) exit; sub(/^# ?/, ""); print }' "$0"
		exit 0
		;;
	*)
		echo "unknown option: $arg" >&2
		exit 2
		;;
	esac
done

root=${WT_DEV_ROOT:-${TMPDIR:-/tmp}/wt-dev}
apps=${WT_APPS_DIR:-$root/apps}
state=${WT_STATE_DIR:-$root/state}
bin=${WT_DEV_BIN:-$root/wt-supervisor}
listen=${WT_LISTEN:-127.0.0.1:8080}
wasmtime=${WT_WASMTIME:-wasmtime}
target=${WT_WASMTIME_TARGET:-x86_64-unknown-linux-gnu}
token=${WT_TOKEN:-dev}

# Reuse the running instance's settings for anything not set explicitly.
pid=$(pgrep -x wt-supervisor | head -n 1 || true)
if [ -n "$pid" ]; then
	from_env() {
		tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n "s/^$1=//p" | head -n 1
	}
	if [ -z "${WT_DEV_BIN:-}" ]; then
		exe=$(readlink -f "/proc/$pid/exe" 2>/dev/null || true)
		if [ -n "$exe" ]; then bin=$exe; fi
	fi
	if [ -z "${WT_APPS_DIR:-}" ]; then
		v=$(from_env WT_APPS_DIR)
		[ -n "$v" ] && apps=$v
	fi
	if [ -z "${WT_STATE_DIR:-}" ]; then
		v=$(from_env WT_STATE_DIR)
		[ -n "$v" ] && state=$v
	fi
	if [ -z "${WT_LISTEN:-}" ]; then
		v=$(from_env WT_LISTEN)
		[ -n "$v" ] && listen=$v
	fi
	if [ -z "${WT_WASMTIME:-}" ]; then
		v=$(from_env WT_WASMTIME)
		[ -n "$v" ] && wasmtime=$v
	fi
	if [ -z "${WT_WASMTIME_TARGET:-}" ]; then
		v=$(from_env WT_WASMTIME_TARGET)
		[ -n "$v" ] && target=$v
	fi
	if [ -z "${WT_TOKEN:-}" ]; then
		v=$(from_env WT_TOKEN)
		[ -n "$v" ] && token=$v
	fi
	echo "dev: reusing $apps and $state"
fi
log=${WT_DEV_LOG:-$(dirname "$apps")/supervisor.log}

mkdir -p "$apps" "$state"

cd "$here"
if [ "$generate" = 1 ]; then
	echo "dev: building the admin panel"
	go generate ./...
fi
echo "dev: building $bin"
go build -o "$bin" .

if [ -n "$pid" ]; then
	echo "dev: stopping $pid"
	kill "$pid" 2>/dev/null || true
	tries=0
	while kill -0 "$pid" 2>/dev/null && [ "$tries" -lt 50 ]; do
		sleep 0.1
		tries=$((tries + 1))
	done
	if kill -0 "$pid" 2>/dev/null; then
		kill -9 "$pid" 2>/dev/null || true
	fi
fi

echo "dev: starting on $listen (log: $log)"
WT_APPS_DIR=$apps \
	WT_STATE_DIR=$state \
	WT_LISTEN=$listen \
	WT_WASMTIME=$wasmtime \
	WT_WASMTIME_TARGET=$target \
	WT_TOKEN=$token \
	nohup "$bin" >>"$log" 2>&1 &

tries=0
while [ "$tries" -lt 100 ]; do
	if curl -fsS "http://$listen/healthz" >/dev/null 2>&1; then
		echo "dev: ready at http://$listen/admin/ (token: $token)"
		exit 0
	fi
	sleep 0.1
	tries=$((tries + 1))
done

echo "dev: supervisor did not answer on $listen; last log lines:" >&2
tail -n 20 "$log" >&2
exit 1
