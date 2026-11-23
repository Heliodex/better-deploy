#!/bin/sh
# End-to-end smoke test for the native Go runtime and a native <-> wasm switch.
#
# Requires wasmtime with compile and serve for the wasm half:
#   WT_WASMTIME=$(command -v wasmtime) ./wt/e2e-native.sh
set -eu

if ! unshare -Ur -m sh -c 'true' 2>/dev/null; then
	echo "e2e-native: skipping: unprivileged user and mount namespaces are unavailable"
	exit 0
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
wasmtime=${WT_WASMTIME:-wasmtime}
port=${WT_E2E_PORT:-18081}
token=${WT_E2E_TOKEN:-e2e-native}
work=$(mktemp -d "${TMPDIR:-/tmp}/wt-e2e-native.XXXXXX")
apps=$work/apps
state=$work/state
log=$work/supervisor.log
mkdir -p "$apps" "$state"

supervisor_pid=

start_supervisor() {
	WT_APPS_DIR=$apps \
	WT_STATE_DIR=$state \
	WT_LISTEN=127.0.0.1:$port \
	WT_WASMTIME=$wasmtime \
	WT_WASMTIME_TARGET=${WT_E2E_TARGET:-x86_64-unknown-linux-gnu} \
	WT_TOKEN=$token \
		"$work/wt-supervisor" >>"$log" 2>&1 &
	supervisor_pid=$!
}

wait_healthy() {
	i=0
	until curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 100 ]; then
			echo "supervisor did not become healthy" >&2
			exit 1
		fi
		sleep 0.2
	done
}

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
		echo "e2e-native failed; supervisor log:" >&2
		tail -n 120 "$log" >&2 || true
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

deploy() {
	archive=$1
	name=$2
	response=$(api -X POST -F "artifact=@$archive" "http://127.0.0.1:$port/apps/$name/deployments")
	id=$(printf '%s' "$response" | json_field id)
	while :; do
		response=$(api "http://127.0.0.1:$port/apps/$name/deployments/$id")
		status=$(printf '%s' "$response" | json_field status)
		case "$status" in
		success)
			return 0
			;;
		failed | canceled)
			message=$(printf '%s' "$response" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("error", ""))')
			echo "deploy $name $status: $message" >&2
			return 1
			;;
		esac
		sleep 0.25
	done
}

rewrite_manifest() {
	python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    data = json.load(f)
data["host"] = "hello.localhost"
data.pop("request_timeout", None)
with open(path, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
PY
}

echo "e2e-native: building supervisor and deploy tool"
(cd "$root/wt/supervisor" && go build -o "$work/wt-supervisor" .)
(cd "$root/wt/deploy" && go build -o "$work/wt-deploy" .)

echo "e2e-native: preparing a standalone native app"
mkdir -p "$work/native-src"
cp "$root/wt/example/hello/main.go" "$work/native-src/main.go"
cp "$root/wt/example/go.sum" "$work/native-src/go.sum"
cp "$root/wt/example/hello/app.json" "$work/native-src/app.json"
rewrite_manifest "$work/native-src/app.json"
cat >"$work/native-src/go.mod" <<EOF
module hello

go 1.27.1

require github.com/heliodex/better-deploy/wt/packages/v2 v2.0.0

replace github.com/heliodex/better-deploy/wt/packages/v2 => $root/wt/packages
EOF

echo "e2e-native: starting supervisor on 127.0.0.1:$port"
start_supervisor
wait_healthy

deploy_native() {
	"$work/wt-deploy" deploy --dir "$work/native-src" --host "http://127.0.0.1:$port" --token "$token" --app-host hello.localhost --runtime native --skip-route-check
}

deploy_wasm() {
	"$work/wt-deploy" deploy --dir "$root/wt/example/hello" --host "http://127.0.0.1:$port" --token "$token" --app-host hello.localhost --runtime wasm --skip-build --skip-route-check
}

echo "e2e-native: deploying native hello"
deploy_native
body=$(curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/")
printf '%s\n' "$body" | grep -q "hello from wt"

info=$(api "http://127.0.0.1:$port/apps/hello")
printf '%s' "$info" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["runtime"] == "native", d; assert d["ready"], d'
pid=$(printf '%s' "$info" | json_field pid)
grep -q "Seccomp:[[:space:]]*2" "/proc/$pid/status"

echo "e2e-native: switching hello to wasm"
deploy_wasm
curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/" | grep -q "hello from wt"
api "http://127.0.0.1:$port/apps/hello" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["runtime"] == "wasm", d; assert d["ready"], d'

echo "e2e-native: switching hello back to native"
deploy_native
curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/" | grep -q "hello from wt"
api "http://127.0.0.1:$port/apps/hello" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["runtime"] == "native", d; assert d["ready"], d'

echo "e2e-native: ok"
