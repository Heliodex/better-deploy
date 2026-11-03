#!/usr/bin/env bash
# Run golangci-lint over every Go module with the repository configuration.
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
config="$root/../.golangci.yml"

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

# run_template scaffolds the embedded app template into a temporary module so
# its Go sources, including the wasip1 wiring, are checked with the same config.
# A stub contract package keeps go mod tidy from resolving the app's own module
# path over the network; contract generation itself is covered by tests.
run_template() {
    local dir
    dir="$(mktemp -d)"
    printf 'lint: deploy/testdata/template (scaffold)\n'
    mkdir -p "$dir/app/contract"
    cat > "$dir/app/contract/contract.go" <<'CONTRACT'
package contract

type Note struct {
	ID        string
	Text      string
	CreatedAt string
}

type NotesResponse struct {
	Notes []Note
}

type CreateNoteRequest struct {
	Text string
}
CONTRACT
    if ! (cd "$root/deploy" && go build -o "$dir/wt" .); then
        rm -rf "$dir"
        status=1
        return
    fi
    if ! "$dir/wt" new --dir "$dir/app" --sdk "$root/packages" --module wt.example/templatelint >/dev/null 2>&1; then
        echo "lint: scaffolding deploy/testdata/template failed" >&2
        rm -rf "$dir"
        status=1
        return
    fi
    (cd "$dir/app" && golangci-lint run --config "$config" "$@" ./...) || status=1
    (cd "$dir/app" && GOOS=wasip1 GOARCH=wasm golangci-lint run --config "$config" --tests=false "$@" ./...) || status=1
    rm -rf "$dir"
}

run "$root/deploy" "$@"
run "$root/supervisor" "$@"
run_component "$root/packages" "$@"
run_component "$root/example" "$@"
run_template "$@"
exit "$status"
