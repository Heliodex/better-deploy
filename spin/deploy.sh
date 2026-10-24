#!/bin/sh
# Builds the Spin app and uploads it to the supervisor's deploy API.
#
# Required environment:
#   DEPLOY_HOST       supervisor host, e.g. deploy.apps.example.com
#   SUPERVISOR_TOKEN  bearer token for the deploy API
#   APP               app name on the supervisor (default: gotime)
#
# app.json is optional. Without it the supervisor keeps the previous one, or
# creates an empty config for a new app.
set -eu

cd "$(dirname "$0")"

: "${DEPLOY_HOST:?set DEPLOY_HOST, e.g. deploy.apps.example.com}"
: "${SUPERVISOR_TOKEN:?set SUPERVISOR_TOKEN}"
APP="${APP:-gotime}"

if [ ! -f app.json ]; then
	echo "warning: no app.json; the server will fall back to defaults" >&2
fi

spin build

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

files="spin.toml main.wasm"
if [ -f app.json ]; then
	files="$files app.json"
fi
# shellcheck disable=SC2086
tar -czf "$tmp/app.tgz" $files

curl -fsS -X POST "https://$DEPLOY_HOST/apps/$APP" \
	-H "Authorization: Bearer $SUPERVISOR_TOKEN" \
	-F "artifact=@$tmp/app.tgz"
echo
