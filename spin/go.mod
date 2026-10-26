module github.com/gotime

// Keep this at or below the patched Go that componentize-go downloads (go1.27.1-wasi-on-idle for v0.4.3).
// With a higher version, Go's automatic toolchain upgrade switches to stock Go, which has no runtime.wasiOnIdle, and linking the wasm component fails with "missing section for relocation target runtime.wasiOnIdle".
go 1.27.1

require (
	github.com/inertia-go/inertia-go v0.10.0
	github.com/spinframework/spin-go-sdk/v3 v3.0.0
)

require (
	github.com/apparentlymart/go-userdirs v0.0.0-20200915174352-b0c018a67c13 // indirect
	github.com/bytecodealliance/componentize-go v0.4.3 // indirect
	github.com/gofrs/flock v0.13.0 // indirect
	go.bytecodealliance.org/pkg v0.2.1 // indirect
	golang.org/x/sys v0.37.0 // indirect
)

tool github.com/bytecodealliance/componentize-go
