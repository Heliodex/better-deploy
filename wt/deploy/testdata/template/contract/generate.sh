#!/bin/sh
# Generate the JSON Schema and Go request types from the Valibot schemas.
set -eu

cd "$(dirname "$0")/.."

(cd web && bun run contract)

go tool go-jsonschema \
	--package contract \
	--struct-name-from-title \
	--capitalization ID \
	-o contract/contract.gen.go \
	contract/schemas/*.json

echo "generated contract/contract.gen.go"
