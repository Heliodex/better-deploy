#!/usr/bin/env bash
# Start SeaweedFS (local S3), deploy the Worker to the celld fleet and serve it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# shellcheck source=dev/env.sh
source dev/env.sh

mkdir -p .local/seaweed/data .local/logs

is_running() {
	[[ -f "$1" ]] && kill -0 "$(cat "$1")" 2>/dev/null
}

# --- SeaweedFS -----------------------------------------------------------------
if ! is_running .local/weed.pid; then
	echo "starting SeaweedFS (S3 on $WEED_S3)..."
	nohup weed server \
		-dir="$root/.local/seaweed/data" \
		-ip=127.0.0.1 -ip.bind=127.0.0.1 \
		-master.port=9333 -volume.port=8082 -filer.port=8888 \
		-s3 -s3.port=8333 -s3.config="$root/dev/seaweed-s3.json" \
		> .local/logs/seaweed.log 2>&1 &
	echo $! > .local/weed.pid
else
	echo "SeaweedFS already running (pid $(cat .local/weed.pid))"
fi

echo -n "waiting for S3 endpoint "
for _ in $(seq 1 60); do
	code="$(curl -s -o /dev/null -w '%{http_code}' "$WEED_S3/" || true)"
	if [[ "$code" == 403 || "$code" == 200 ]]; then
		echo "ready"
		break
	fi
	echo -n "."
	sleep 1
done
if [[ "${code:-}" != 403 && "${code:-}" != 200 ]]; then
	echo
	echo "SeaweedFS S3 did not come up; see .local/logs/seaweed.log" >&2
	exit 1
fi

# Buckets are metadata in the filer; create the fleet bucket if it is missing.
if ! printf 's3.bucket.list\n' | weed shell -master="$WEED_MASTER" 2>/dev/null | grep -q "^[[:space:]]*$S3_BUCKET[[:space:]]"; then
	echo "creating bucket $S3_BUCKET"
	printf 's3.bucket.create -name %s\n' "$S3_BUCKET" | weed shell -master="$WEED_MASTER"
fi

echo "bucket check:"
celld diagnose \
	--bucket "$CELLD_BUCKET" --endpoint "$S3_ENDPOINT" \
	--listen "$CELLD_LISTEN" --internal-listen "$CELLD_INTERNAL_LISTEN"

# --- celld ---------------------------------------------------------------------
# Deploy before starting the node so the first request is served immediately.
celld deploy "$root" --bucket "$CELLD_BUCKET" --endpoint "$S3_ENDPOINT"

if ! is_running .local/celld.pid; then
	echo "starting celld on $CELLD_LISTEN..."
	nohup celld \
		--bucket "$CELLD_BUCKET" --endpoint "$S3_ENDPOINT" \
		--listen "$CELLD_LISTEN" --internal-listen "$CELLD_INTERNAL_LISTEN" \
		> .local/logs/celld.log 2>&1 &
	echo $! > .local/celld.pid
else
	echo "celld already running (pid $(cat .local/celld.pid)); it adopts the new deployment"
fi

echo -n "waiting for the app "
for _ in $(seq 1 60); do
	if out="$(curl -sS -m 5 "http://$CELLD_LISTEN/?name=dev" 2>/dev/null)"; then
		echo "ready"
		echo
		echo "$out"
		echo
		echo "App:        http://$CELLD_LISTEN/?name=dev"
		echo "S3 bucket:  $CELLD_BUCKET at $S3_ENDPOINT"
		echo "Logs:       .local/logs/celld.log, .local/logs/seaweed.log"
		exit 0
	fi
	echo -n "."
	sleep 1
done
echo
echo "celld did not serve the app; see .local/logs/celld.log" >&2
exit 1
