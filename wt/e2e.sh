#!/bin/sh
# End-to-end smoke test: build a real component, deploy it, proxy a request, snapshot and restore /data, then roll back.
#
# Requires a wasmtime binary with the compile and serve subcommands:
#   WT_WASMTIME=$(command -v wasmtime) ./wt/e2e.sh
#
# The default target matches a released glibc wasmtime build. Set WT_E2E_TARGET for another runtime.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
wasmtime=${WT_WASMTIME:-wasmtime}
target=${WT_E2E_TARGET:-x86_64-unknown-linux-gnu}
port=${WT_E2E_PORT:-18080}
token=${WT_E2E_TOKEN:-e2e}
work=$(mktemp -d "${TMPDIR:-/tmp}/wt-e2e.XXXXXX")
apps=$work/apps
state=$work/state
log=$work/supervisor.log
mkdir -p "$apps" "$state"

supervisor_pid=

cleanup() {
	if [ -n "$supervisor_pid" ]; then
		kill "$supervisor_pid" 2>/dev/null || true
		wait "$supervisor_pid" 2>/dev/null || true
	fi
}

on_exit() {
	status=$?
	cleanup
	if [ "$status" -ne 0 ]; then
		echo "e2e failed; supervisor log:" >&2
		tail -n 100 "$log" >&2 || true
		echo "work directory kept at $work" >&2
	fi
	exit "$status"
}
trap on_exit EXIT

api() {
	curl -fsS -H "Authorization: Bearer $token" "$@"
}

json_field() {
	python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

echo "e2e: building supervisor and deploy tool"
(cd "$root/wt/supervisor" && go build -o "$work/wt-supervisor" .)
(cd "$root/wt/deploy" && go build -o "$work/wt-deploy" .)

echo "e2e: bundling hello for $target"
"$work/wt-deploy" bundle --dir "$root/wt/example/hello" --out "$work/hello.tar.gz" --wasmtime "$wasmtime" --target "$target"

echo "e2e: starting supervisor on 127.0.0.1:$port"
WT_APPS_DIR=$apps \
WT_STATE_DIR=$state \
WT_LISTEN=127.0.0.1:$port \
WT_WASMTIME=$wasmtime \
WT_WASMTIME_TARGET=$target \
WT_TOKEN=$token \
	"$work/wt-supervisor" >"$log" 2>&1 &
supervisor_pid=$!

i=0
until curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 100 ]; then
		echo "supervisor did not become healthy" >&2
		exit 1
	fi
	sleep 0.2
done

echo "e2e: deploying hello"
api -X POST -F "artifact=@$work/hello.tar.gz" "http://127.0.0.1:$port/apps/hello" >/dev/null
body=$(curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/")
echo "$body" | grep -q "hello from wt"
echo "$body" | grep -q "path=/"

echo "e2e: snapshotting /data"
mkdir -p "$state/hello"
printf keep >"$state/hello/data.txt"
backups=$(api -X POST "http://127.0.0.1:$port/apps/hello/backups")
snapshot=$(printf '%s' "$backups" | json_field created)
if [ -z "$snapshot" ]; then
	echo "no snapshot was created: $backups" >&2
	exit 1
fi
printf changed >"$state/hello/data.txt"
api -X POST "http://127.0.0.1:$port/apps/hello/backups/$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$snapshot")/restore" >/dev/null
grep -q keep "$state/hello/data.txt"

echo "e2e: rolling back the retained deployment"
deployments=$(api "http://127.0.0.1:$port/apps/hello/deployments")
deployment=$(printf '%s' "$deployments" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
api -X POST "http://127.0.0.1:$port/apps/hello/rollback/$deployment" >/dev/null
curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/" | grep -q "hello from wt"
grep -q keep "$state/hello/data.txt"

echo "e2e: ok"
