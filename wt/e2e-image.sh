#!/bin/sh
# Optional end-to-end check against the actual musl runtime image.
# Requires podman and a wasmtime build that can compile for x86_64-unknown-linux-musl.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${WT_E2E_IMAGE:-wt-e2e:local}
port=${WT_E2E_IMAGE_PORT:-18081}
token=${WT_E2E_TOKEN:-image-e2e}
wasmtime=${WT_WASMTIME:-wasmtime}
work=$(mktemp -d "${TMPDIR:-/tmp}/wt-image-e2e.XXXXXX")
container=

cleanup() {
	if [ -n "$container" ]; then
		podman rm -f "$container" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

echo "e2e-image: building $image"
podman build -f "$root/wt/Containerfile" -t "$image" "$root"

echo "e2e-image: starting the container"
container=$(podman run -d --rm -p "127.0.0.1:$port:8080" -e "WT_TOKEN=$token" "$image")
i=0
until curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 100 ]; then
		echo "container supervisor did not become healthy" >&2
		exit 1
	fi
	sleep 0.2
done

echo "e2e-image: building and deploying hello for musl"
(cd "$root/wt/deploy" && go build -o "$work/wt-deploy" .)
"$work/wt-deploy" bundle --dir "$root/wt/example/hello" --out "$work/hello.tar.gz" --wasmtime "$wasmtime" --target x86_64-unknown-linux-musl
curl -fsS -H "Authorization: Bearer $token" -X POST -F "artifact=@$work/hello.tar.gz" "http://127.0.0.1:$port/apps/hello" >/dev/null
curl -fsS -H "Host: hello.localhost" "http://127.0.0.1:$port/" | grep -q "hello from wt"

echo "e2e-image: ok"
