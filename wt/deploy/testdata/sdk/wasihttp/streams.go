package wasihttp

import (
	"fmt"
	"io"

	wit "go.bytecodealliance.org/pkg/wit/types"
	wasi "wt.packages/binding/imports/wasi_http_0_3_0_types"
)

// Assert `bodyReader` implements the required interfaces
var _ io.Reader = &bodyReader{}

var _ io.ReadCloser = &bodyReader{}

type bodyReader struct {
	stream   *wit.StreamReader[uint8]
	trailers *wit.FutureReader[wit.Result[wit.Option[*wasi.Fields], wasi.ErrorCode]]
}

func (r *bodyReader) takeError() error {
	if r.trailers != nil {
		trailers := r.trailers.Read()
		r.trailers = nil
		if trailers.IsErr() {
			return fmt.Errorf("failed to read from HTTP body stream: %s", errorString(trailers.Err()))
		}
	}
	return io.EOF
}

func (r *bodyReader) Read(p []byte) (n int, err error) {
	if r.stream.WriterDropped() {
		return 0, r.takeError()
	}

	count := r.stream.Read(p)
	if count == 0 && r.stream.WriterDropped() {
		return 0, r.takeError()
	}

	return int(count), nil
}

func (r *bodyReader) Close() error {
	if r.stream != nil {
		r.stream.Drop()
	}
	if r.trailers != nil {
		r.trailers.Drop()
	}
	return nil
}

// newReader creates an io.Reader from the input stream.
func newReader(
	stream *wit.StreamReader[uint8],
	trailers *wit.FutureReader[wit.Result[wit.Option[*wasi.Fields], wasi.ErrorCode]],
) *bodyReader {
	return &bodyReader{
		stream:   stream,
		trailers: trailers,
	}
}
