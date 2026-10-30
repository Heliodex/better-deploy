package wasihttp

import (
	"fmt"
	"net/http"

	wit "go.bytecodealliance.org/pkg/wit/types"
	wasi "wt.packages/binding/imports/wasi_http_0_3_0_types"
)

// Assert that `responseWriter` implements the required interface
var _ http.ResponseWriter = &responseWriter{}
var _ http.Flusher = &responseWriter{}

type responseWriter struct {
	// channel to which the response will be sent
	channel chan wit.Result[*wasi.Response, wasi.ErrorCode]
	// stream to which the response body is being written
	stream *wit.StreamWriter[uint8]
	// future which resolves to an error if there is a problem delivering the response body
	streamResult *wit.FutureReader[wit.Result[wit.Unit, wasi.ErrorCode]]
	// headers to send
	headers http.Header
	// status code to send
	statusCode int
}

func newHttpResponseWriter() *responseWriter {
	return &responseWriter{
		channel:    make(chan wit.Result[*wasi.Response, wasi.ErrorCode]),
		headers:    http.Header{},
		statusCode: 200,
	}
}

func (w *responseWriter) Header() http.Header {
	return w.headers
}

func (w *responseWriter) send() error {
	if w.channel == nil {
		return nil
	}

	fields, err := toWasiHeaders(w.headers)
	if err != nil {
		return err
	}

	channel := w.channel
	w.channel = nil

	tx, rx := wasi.MakeStreamU8()
	w.stream = tx

	response, send := wasi.ResponseNew(
		fields,
		wit.Some(rx),
		trailersFuture(), // TODO: support trailers
	)
	w.streamResult = send

	response.SetStatusCode(uint16(w.statusCode))

	channel <- wit.Ok[*wasi.Response, wasi.ErrorCode](response)

	return nil
}

func (r *responseWriter) takeError() error {
	if r.streamResult != nil {
		streamResult := r.streamResult.Read()
		r.streamResult = nil
		if streamResult.IsErr() {
			return fmt.Errorf(
				"failed to read from HTTP body stream: %s",
				errorString(streamResult.Err()),
			)
		}
	}
	return nil
}

func (w *responseWriter) Write(buf []byte) (int, error) {
	err := w.send()
	if err != nil {
		return 0, err
	}

	count := w.stream.Write(buf)

	if count == 0 && w.stream.ReaderDropped() {
		return 0, w.takeError()
	}

	return int(count), nil
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Flush starts the response before the handler writes a body.
func (w *responseWriter) Flush() {
	_ = w.send()
}

func (w *responseWriter) close() {
	if w.stream != nil {
		w.stream.Drop()
	}
	if w.streamResult != nil {
		w.streamResult.Drop()
	}
}
