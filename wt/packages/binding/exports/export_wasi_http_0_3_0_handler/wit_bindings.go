package export_wasi_http_0_3_0_handler

import (
	. "go.bytecodealliance.org/pkg/wit/types"
	. "github.com/heliodex/better-deploy/wt/packages/v2/binding/imports/wasi_http_0_3_0_types"
)

// Exports is the hook the generated async export wrapper calls. A handler package sets Exports.Handle at init time.
var Exports struct {
	Handle func(request *Request) Result[*Response, ErrorCode]
}

func Handle(request *Request) Result[*Response, ErrorCode] {
	return Exports.Handle(request)
}
