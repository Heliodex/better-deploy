// Package contract holds the request types generated from the Valibot schemas. Regenerate them with go generate ./contract/ from the app directory.
package contract

//go:generate sh -c "cd ../web && bun run contract"
//go:generate sh -c "go tool go-jsonschema --package contract --struct-name-from-title --capitalization ID -o contract.gen.go schemas/*.json"
