#!/usr/bin/env bash
# 200 only once the node serves; 503 while it joins, settles or drains.
set -uo pipefail

addr=${CELLD_ADDR:-0.0.0.0:8080}
port=${addr##*:}

curl -fsS --max-time 3 "http://127.0.0.1:$port/.well-known/celld/health"
