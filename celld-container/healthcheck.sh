#!/usr/bin/env bash
# 200 only once the node serves; 503 while it joins, settles or drains.
# Uses bash's /dev/tcp so the final image needs no curl.
set -uo pipefail

addr=${CELLD_ADDR:-0.0.0.0:8080}
port=${addr##*:}

exec 3<>"/dev/tcp/127.0.0.1/$port" || exit 1
printf 'GET /.well-known/celld/health HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n' >&3
IFS= read -r status <&3 || exit 1
exec 3<&-

case $status in
*" 200 "*) exit 0 ;;
esac
printf 'celld health: %s\n' "$status" >&2
exit 1
