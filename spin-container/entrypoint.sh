#!/bin/sh
# Prepares the writable directories and starts the supervisor.
#
# A freshly mounted volume arrives owned by root. When this script is started as root it takes the directories the supervisor and its children write to, hands them to uid 1000, and re-execs itself as that user. An image started as 1000 skips the first branch.
set -eu

log() { printf '%s entrypoint: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() {
	log "$*"
	exit 1
}

: "${HOME:=/var/lib/spin/home}"
: "${TMPDIR:=/var/lib/spin/tmp}"
: "${XDG_DATA_HOME:=/var/lib/spin/caddy/data}"
: "${XDG_CONFIG_HOME:=/var/lib/spin/caddy/config}"
: "${SPIN_APPS_DIR:=/var/lib/spin/apps}"
: "${SPIN_STATE_DIR:=/var/lib/spin/state}"
: "${SPIN_LOG_DIR:=/var/lib/spin/logs}"
: "${SPIN_CACHE_DIR:=/var/lib/spin/cache}"
: "${SPIN_TMP_DIR:=$TMPDIR}"
: "${CADDY_CONFIG:=/var/lib/spin/caddy/Caddyfile}"
: "${CADDY_CONFIG_DIR:=$XDG_CONFIG_HOME}"
: "${CADDY_DATA_DIR:=$XDG_DATA_HOME}"

export HOME TMPDIR XDG_DATA_HOME XDG_CONFIG_HOME
export SPIN_APPS_DIR SPIN_STATE_DIR SPIN_LOG_DIR SPIN_CACHE_DIR SPIN_TMP_DIR
export CADDY_CONFIG CADDY_CONFIG_DIR CADDY_DATA_DIR

dirs="$HOME $TMPDIR $SPIN_APPS_DIR $SPIN_STATE_DIR $SPIN_LOG_DIR $SPIN_CACHE_DIR $SPIN_TMP_DIR $CADDY_CONFIG_DIR $CADDY_DATA_DIR $(dirname "$CADDY_CONFIG")"

# These paths are chowned recursively when the container starts as root, so a relative path or / would be catastrophic. Require absolute directories.
for dir in $dirs; do
	case "$dir" in
	/*) ;;
	*) die "path must be absolute: $dir" ;;
	esac
	[ "$dir" = "/" ] && die "refusing to use / as a writable directory"
done

if [ "$(id -u)" = 0 ]; then
	for dir in $dirs; do
		mkdir -p "$dir" || die "cannot create $dir"
		chown -R 1000:1000 "$dir" || die "cannot hand $dir to uid 1000"
	done
	exec su-exec 1000:1000 "$0" "$@"
fi

for dir in $dirs; do
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir"
	[ -w "$dir" ] || die "$dir is not writable by uid $(id -u); use a volume it owns"
done

exec /usr/local/bin/supervisor "$@"
