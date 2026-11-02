package wasihttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	wit "go.bytecodealliance.org/pkg/wit/types"
	wasi "wt.packages/binding/imports/wasi_http_0_3_0_types"
)

// toWasiMethod maps an HTTP method name to a wasi:http method, using the "other" variant for extensions.
func toWasiMethod(s string) wasi.Method {
	switch s {
	case http.MethodConnect:
		return wasi.MakeMethodConnect()
	case http.MethodDelete:
		return wasi.MakeMethodDelete()
	case http.MethodGet:
		return wasi.MakeMethodGet()
	case http.MethodHead:
		return wasi.MakeMethodHead()
	case http.MethodOptions:
		return wasi.MakeMethodOptions()
	case http.MethodPatch:
		return wasi.MakeMethodPatch()
	case http.MethodPost:
		return wasi.MakeMethodPost()
	case http.MethodPut:
		return wasi.MakeMethodPut()
	case http.MethodTrace:
		return wasi.MakeMethodTrace()
	default:
		return wasi.MakeMethodOther(s)
	}
}

// outgoingRequestOptions turns the request context deadline into wasi:http transport timeouts. The caller keeps the context as the cancellation signal.
func outgoingRequestOptions(ctx context.Context) wit.Option[*wasi.RequestOptions] {
	deadline, ok := ctx.Deadline()
	if !ok {
		return wit.None[*wasi.RequestOptions]()
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	options := wasi.MakeRequestOptions()
	duration := wit.Some(uint64(remaining))
	options.SetConnectTimeout(duration)
	options.SetFirstByteTimeout(duration)
	options.SetBetweenBytesTimeout(duration)
	return wit.Some(options)
}

// newOutgoingHttpRequest converts an http.Request to a wasi.Request. The body copier stops when the request context is cancelled.
func newOutgoingHttpRequest(req *http.Request) (*wasi.Request, error) {
	headers, err := toWasiHeaders(req.Header)
	if err != nil {
		return nil, err
	}
	defer headers.Drop()

	ctx := req.Context()
	var body wit.Option[*wit.StreamReader[uint8]]
	if req.Body == nil {
		body = wit.None[*wit.StreamReader[uint8]]()
	} else {
		tx, rx := wasi.MakeStreamU8()
		body = wit.Some(rx)
		go func() {
			defer tx.Drop()
			defer req.Body.Close()

			buffer := make([]uint8, 16*1024)
			for !tx.ReaderDropped() {
				if ctx.Err() != nil {
					return
				}
				count, err := req.Body.Read(buffer)
				if err != nil {
					if err != io.EOF {
						fmt.Printf("error reading request body: %v", err)
					}
					return
				}
				tx.WriteAll(buffer[:count])
			}
		}()
	}

	request, send := wasi.RequestNew(
		headers,
		body,
		trailersFuture(), // TODO: support trailers
		outgoingRequestOptions(ctx),
	)
	send.Drop()
	authority := req.Host
	if authority == "" {
		authority = req.URL.Host
	}
	request.SetMethod(toWasiMethod(req.Method))
	request.SetAuthority(wit.Some(authority))
	request.SetPathWithQuery(wit.Some(req.URL.RequestURI()))

	switch req.URL.Scheme {
	case "http":
		request.SetScheme(wit.Some(wasi.MakeSchemeHttp()))
	case "https":
		request.SetScheme(wit.Some(wasi.MakeSchemeHttps()))
	default:
		request.SetScheme(wit.Some(wasi.MakeSchemeOther(req.URL.Scheme)))
	}

	return request, nil
}
