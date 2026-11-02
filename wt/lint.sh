#!/usr/bin/env bash
# Run golangci-lint over every Go module with the repository configuration.
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
config="$root/../.golangci.yml"
if [[ ${1-} == "--all" ]]; then
    config="$root/../.golangci-all.yml"
    shift
fi

if ! command -v golangci-lint >/dev/null 2>&1; then
    echo "golangci-lint is not installed" >&2
    exit 1
fi

golangci-lint config verify --config "$config"

status=0
run() {
    local dir="$1"
    shift
    printf 'lint: %s\n' "${dir#"$root"/}"
    (cd "$dir" && golangci-lint run --config "$config" "$@" ./...) || status=1
}

run_component() {
    local dir="$1"
    shift
    printf 'lint: %s (wasip1)\n' "${dir#"$root"/}"
    (cd "$dir" && GOOS=wasip1 GOARCH=wasm golangci-lint run --config "$config" "$@" ./...) || status=1
}

run "$root/deploy" "$@"
run "$root/supervisor" "$@"
run_component "$root/packages" "$@"
run_component "$root/example" "$@"
exit "$status"
