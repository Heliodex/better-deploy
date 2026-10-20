#!/usr/bin/env bash
# Stop the celld node and SeaweedFS started by dev/start.sh.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

for name in celld weed; do
	pidfile=".local/$name.pid"
	[[ -f "$pidfile" ]] || continue
	pid="$(cat "$pidfile")"
	if kill -0 "$pid" 2>/dev/null; then
		echo "stopping $name (pid $pid)..."
		kill "$pid"
		for _ in $(seq 1 30); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 1
		done
		kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
	fi
	rm -f "$pidfile"
done
