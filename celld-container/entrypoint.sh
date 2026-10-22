#!/usr/bin/env bash
# Runs the Versity S3 Gateway and one celld node in a single container.
#
# The gateway is the fleet bucket. The node is the only process that talks to it, except for `celld deploy` (which can reach it through the published port). If either process dies, the other is stopped, so the container reports the failure instead of half-working.
set -euo pipefail

log() { printf '%s entrypoint: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

S3_DATA_DIR=${S3_DATA_DIR:-/var/lib/celld/s3}
IAM_DIR=${IAM_DIR:-/var/lib/celld/iam}
SIDECAR_DIR=${SIDECAR_DIR:-/var/lib/celld/sidecar}
VGW_PORT=${VGW_PORT:-10000}
STOP_GRACE_S=${STOP_GRACE_S:-75}

export CELLD_WATCH=${CELLD_WATCH:-/var/lib/celld/work}
export CELLD_BUCKET=${CELLD_BUCKET:-s3://celld}

# Test credentials by default. Anyone who can reach the S3 port controls the
# whole fleet, so publish it to loopback only, or set real credentials.
VGW_ACCESS_KEY_ID=${VGW_ACCESS_KEY_ID:-testuser}
VGW_SECRET_ACCESS_KEY=${VGW_SECRET_ACCESS_KEY:-testsecret}

export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-$VGW_ACCESS_KEY_ID}
export AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-$VGW_SECRET_ACCESS_KEY}
export AWS_REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}
export AWS_DEFAULT_REGION=$AWS_REGION
export S3_ENDPOINT=${S3_ENDPOINT:-http://127.0.0.1:$VGW_PORT}

# A non-loopback public listener requires an explicit internal listener. Keep
# the internal one on loopback: it has no authentication and peer traffic is
# not encrypted, so it must not be published.
export CELLD_ADDR=${CELLD_ADDR:-0.0.0.0:8080}
export CELLD_INTERNAL_ADDR=${CELLD_INTERNAL_ADDR:-127.0.0.1:8081}

# versitygw treats every top-level directory as a bucket, so the bucket celld
# asks for has to exist before the node probes the store.
bucket=${CELLD_BUCKET#*://}
bucket=${bucket%%/*}
[[ -n $bucket ]] || die "CELLD_BUCKET=$CELLD_BUCKET has no bucket name"

mkdir -p "$S3_DATA_DIR/$bucket" "$IAM_DIR" "$CELLD_WATCH" "$SIDECAR_DIR" \
	|| die "cannot create data directories under $S3_DATA_DIR"

vgw_args=(
	--access "$VGW_ACCESS_KEY_ID"
	--secret "$VGW_SECRET_ACCESS_KEY"
	--region "$AWS_REGION"
	--port "0.0.0.0:$VGW_PORT"
	--iam-dir "$IAM_DIR"
)
# The node polls the deployment pointer through the gateway, so request logs
# are mostly noise; VGW_REQUEST_LOGS=1 keeps them for debugging.
if [[ ${VGW_REQUEST_LOGS:-0} != 1 ]]; then
	vgw_args+=(--quiet)
fi
# Flags after `posix` belong to the backend, not the gateway itself.
vgw_posix_args=(
	# The posix backend keeps checksums and metadata in user.* xattrs by default, which fails on filesystems without xattr support (e.g. some overlay / network volumes) with `user.checksums: operation not supported`. A sidecar directory stores the same metadata as files and works everywhere.
	--sidecar "$SIDECAR_DIR"
)
# celld proves a write is durable, so an fsync per object is only needed when
# the container's filesystem itself can lose acknowledged writes.
if [[ ${VGW_FSYNC:-0} == 1 ]]; then
	vgw_posix_args+=(--fsync)
fi

log "starting versitygw on :$VGW_PORT, data in $S3_DATA_DIR"
versitygw "${vgw_args[@]}" posix "${vgw_posix_args[@]}" "$S3_DATA_DIR" &
vgw_pid=$!

# A successful TCP connect means the listener is up; the gateway rejects the unauthenticated request afterwards, which is fine. Bash's /dev/tcp keeps curl out of the final image.
ready=0
for _ in $(seq 1 300); do
	if ! kill -0 "$vgw_pid" 2>/dev/null; then
		wait "$vgw_pid" || true
		die "versitygw exited while starting"
	fi
	if { exec 3<>"/dev/tcp/127.0.0.1/$VGW_PORT"; } 2>/dev/null; then
		exec 3<&-
		ready=1
		break
	fi
	sleep 0.2
done
[[ $ready == 1 ]] || die "versitygw did not accept connections on :$VGW_PORT"

log "starting celld: bucket=$CELLD_BUCKET endpoint=$S3_ENDPOINT listen=$CELLD_ADDR"
# Every setting is an environment variable, so CELLD_* tuning passes straight
# through; extra arguments are appended to the node command.
celld "$@" &
celld_pid=$!

shutdown() {
	local code=$1
	trap - TERM INT
	if kill -0 "$celld_pid" 2>/dev/null; then
		log "stopping celld, allowing ${STOP_GRACE_S}s for the drain"
		kill -TERM "$celld_pid" 2>/dev/null || true
		for ((i = 0; i < STOP_GRACE_S; i++)); do
			kill -0 "$celld_pid" 2>/dev/null || break
			sleep 1
		done
		kill -KILL "$celld_pid" 2>/dev/null || true
	fi
	kill -TERM "$vgw_pid" 2>/dev/null || true
	wait "$celld_pid" 2>/dev/null || true
	wait "$vgw_pid" 2>/dev/null || true
	exit "$code"
}
trap 'shutdown 143' TERM
trap 'shutdown 130' INT

while :; do
	if ! kill -0 "$celld_pid" 2>/dev/null; then
		code=0
		wait "$celld_pid" || code=$?
		log "celld exited with status $code"
		shutdown "$code"
	fi
	if ! kill -0 "$vgw_pid" 2>/dev/null; then
		code=0
		wait "$vgw_pid" || code=$?
		log "versitygw exited with status $code"
		shutdown "$code"
	fi
	sleep 1
done
