module wt.example

go 1.27.1

require wt.packages v0.0.0

replace wt.packages => ../packages

require (
	dario.cat/mergo v1.0.2 // indirect
	github.com/apparentlymart/go-userdirs v0.0.0-20200915174352-b0c018a67c13 // indirect
	github.com/atombender/go-jsonschema v0.24.1 // indirect
	github.com/bytecodealliance/componentize-go v0.4.3 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/sanity-io/litter v1.5.8 // indirect
	github.com/sosodev/duration v1.4.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	go.bytecodealliance.org/pkg v0.4.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
)

tool (
	github.com/atombender/go-jsonschema
	github.com/bytecodealliance/componentize-go
)
