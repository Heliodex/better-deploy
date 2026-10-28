#!/bin/sh
# Regenerate the WASI bindings for the wt:app/app world. The output under binding/ is not tracked; the example apps call this script before building.
set -eu

cd "$(dirname "$0")"

rm -rf binding tmp

go tool componentize-go --ignore-toml-files -w wt:app/app -d wit \
	bindings --format -o binding/imports \
	--pkg-name wt.packages/binding/imports --include-versions
rm -rf binding/imports/wit_exports

go tool componentize-go --ignore-toml-files -w wt:app/app -d wit \
	bindings --format -o tmp \
	--export-pkg-name wt.packages/binding/exports \
	--pkg-name wt.packages/binding/imports --include-versions
mkdir -p binding/exports
cp -r tmp/wit_exports binding/exports/
rm -rf tmp

# The export stub is hand-written because componentize-go generates the async export wrapper but not the package it calls.
mkdir -p binding/exports/export_wasi_http_0_3_0_handler
cat > binding/exports/export_wasi_http_0_3_0_handler/wit_bindings.go <<'EOF'
package export_wasi_http_0_3_0_handler

import (
	. "go.bytecodealliance.org/pkg/wit/types"
	. "wt.packages/binding/imports/wasi_http_0_3_0_types"
)

// Exports is the hook the generated async export wrapper calls. A handler package sets Exports.Handle at init time.
var Exports struct {
	Handle func(request *Request) Result[*Response, ErrorCode]
}

func Handle(request *Request) Result[*Response, ErrorCode] {
	return Exports.Handle(request)
}
EOF

echo "generated bindings in $(pwd)/binding"
