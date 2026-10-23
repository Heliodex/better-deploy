#!/usr/bin/env bash
# Serves the Spin application mounted at /apps.
#
# `spin watch --skip-build` restarts the application when its manifest or any
# component source changes, so rebuilding on the host (or copying a build into
# the mounted volume) takes effect without restarting the container. If watch
# exits - a half-copied upload, a manifest that does not parse, a missing wasm -
# this loop waits for a usable application and tries again, rather than letting
# the container die.
set -euo pipefail

log() { printf '%s entrypoint: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() {
	log "$*"
	exit 1
}

SPIN_APP=${SPIN_APP:-/apps}
SPIN_LISTEN=${SPIN_LISTEN:-0.0.0.0:3000}
SPIN_WATCH=${SPIN_WATCH:-1}
SPIN_STATE_DIR=${SPIN_STATE_DIR:-/var/lib/spin/state}
SPIN_LOG_DIR=${SPIN_LOG_DIR:-/var/lib/spin/logs}
SPIN_CACHE_DIR=${SPIN_CACHE_DIR:-/var/lib/spin/cache}

case "$SPIN_WATCH" in
0 | 1) ;;
*) die "SPIN_WATCH must be 0 or 1, got '$SPIN_WATCH'" ;;
esac

# A freshly mounted volume arrives owned by root. Take the directories Spin
# writes to, hand them to uid 1000, and re-exec this script as that user; nothing
# below runs with privileges. An image started as 1000 skips this.
if [[ $EUID -eq 0 ]]; then
	for dir in "$SPIN_STATE_DIR" "$SPIN_LOG_DIR" "$SPIN_CACHE_DIR" /var/lib/spin/home /var/lib/spin/tmp; do
		mkdir -p "$dir" || die "cannot create $dir"
		chown 1000:1000 "$dir" || die "cannot hand $dir to uid 1000"
	done
	exec su-exec 1000:1000 "$0" "$@"
fi

for dir in "$SPIN_STATE_DIR" "$SPIN_LOG_DIR" "$SPIN_CACHE_DIR" /var/lib/spin/home /var/lib/spin/tmp; do
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir"
	[[ -w $dir ]] || die "$dir is not writable by uid $(id -u); use a volume it owns"
done

# A path to the manifest, whether SPIN_APP names the directory or the file.
if [[ $SPIN_APP == *.toml ]]; then
	manifest=$SPIN_APP
else
	manifest=$SPIN_APP/spin.toml
fi

stopping=0
child=0

shutdown() {
	stopping=1
	if [[ $child -ne 0 ]]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap shutdown TERM INT

# Sleeps that a stop signal can interrupt promptly.
nap() {
	sleep "$1" &
	wait $! || true
}

wait_for_app() {
	[[ -e $manifest ]] && return 0
	log "waiting for an application: $manifest does not exist yet"
	while [[ ! -e $manifest ]]; do
		nap 2
		[[ $stopping -eq 1 ]] && return 1
	done
	log "using the application at $SPIN_APP"
}

# These select the runtime, not the application; every other variable in the
# container is forwarded to the components (below).
control=(SPIN_APP SPIN_LISTEN SPIN_WATCH SPIN_STATE_DIR SPIN_LOG_DIR SPIN_CACHE_DIR)

delay=1
while :; do
	[[ $stopping -eq 1 ]] && exit 143
	wait_for_app || exit 143

	if [[ $SPIN_WATCH -eq 1 ]]; then
		args=(spin watch --skip-build)
	else
		args=(spin up)
	fi
	args+=(
		-f "$SPIN_APP"
		--listen "$SPIN_LISTEN"
		--state-dir "$SPIN_STATE_DIR"
		--log-dir "$SPIN_LOG_DIR"
		--cache-dir "$SPIN_CACHE_DIR"
	)

	# Spin gives components an empty environment unless values are passed with
	# -e, so a plain `podman run -e session_key=...` would not reach the guest.
	# Forward the container's environment, minus the controls above. Arguments
	# after the image name are appended, for flags such as --runtime-config-file
	# or --variable.
	while IFS= read -r name; do
		for skip in "${control[@]}"; do
			[[ $name == "$skip" ]] && continue 2
		done
		args+=(-e "$name=${!name}")
	done < <(compgen -e)
	args+=("$@")

	started=$SECONDS
	if [[ $SPIN_WATCH -eq 1 ]]; then
		log "starting spin watch on $SPIN_LISTEN, app $SPIN_APP, state $SPIN_STATE_DIR"
	else
		log "starting spin up on $SPIN_LISTEN, app $SPIN_APP, state $SPIN_STATE_DIR"
	fi
	"${args[@]}" &
	child=$!

	code=0
	wait "$child" || code=$?

	# Let Spin finish the shutdown its own TERM started, so an in-flight request
	# is not cut off by the container disappearing underneath it.
	if [[ $stopping -eq 1 ]]; then
		log "shutting down"
		wait "$child" 2>/dev/null || true
		exit 143
	fi
	child=0

	# A broken application would otherwise restart in a tight loop. Back off
	# while it stays broken, but start almost immediately after a long run.
	if ((SECONDS - started >= 30)); then
		delay=1
	else
		delay=$((delay * 2))
		((delay > 30)) && delay=30
	fi
	log "spin exited with status $code; retrying in ${delay}s"
	nap "$delay"
done
